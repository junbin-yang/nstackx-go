// CoAP协议的组播通信支持，核心用于设备发现场景
package coap

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/network"
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/plgd-dev/go-coap/v3/mux"
	coapNet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"
	"github.com/plgd-dev/go-coap/v3/udp/server"
)

const (
	// CoAP组播地址常量（符合RFC 7252标准）
	MulticastIPv4Addr = "224.0.1.187" // IPv4站点本地组播地址（CoAP标准设备发现地址）
	MulticastIPv6Addr = "ff02::fd"    // IPv6链路本地“所有节点”组播地址
	CoAPPort          = 5683          // CoAP协议默认端口（UDP）

	// 网络参数常量
	DefaultTTL = 255 // IPv4组播默认TTL（生存时间，控制消息传播范围）
)

// MulticastHandler 组播通信处理器，管理组播连接、接口、消息队列等核心资源
type MulticastHandler struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问时的资源安全

	ipv4Conn   *ipv4.PacketConn   // IPv4组播数据包连接（基于x/net/ipv4封装）
	ipv6Conn   *ipv6.PacketConn   // IPv6组播数据包连接（基于x/net/ipv6封装）
	udpConn4   *net.UDPConn       // 原始IPv4 UDP连接（供ipv4Conn底层使用）
	udpConn6   *net.UDPConn       // 原始IPv6 UDP连接（供ipv6Conn底层使用）
	coapConn4  *coapNet.UDPConn   // COAP包提供的UDP连接
	coapConn6  *coapNet.UDPConn   // COAP包提供的UDP连接
	interfaces []net.Interface    // 支持组播的网络接口列表（已筛选）
	ctx        context.Context    // 上下文，用于协程退出控制
	cancel     context.CancelFunc // 上下文取消函数，触发处理器停止
	log        *logger.Logger     // 日志实例（基于zap框架）
	netmgr     *network.Manager   // 网络管理器（统一接口来源）
	udpServer  *server.Server     // 基于go-coap的UDP服务器
	router     *mux.Router        // 共享路由，供server.go注入
}

// MulticastConfig 组播配置结构体，用于初始化MulticastHandler时自定义参数
type MulticastConfig struct {
	IPv4Enabled bool     // 是否启用IPv4组播
	IPv6Enabled bool     // 是否启用IPv6组播
	Interfaces  []string // 指定使用的网络接口名称（空则自动筛选所有符合条件的接口）
	Port        int      // 组播监听/发送端口（默认使用CoAPPort=5683）
	TTL         int      // 组播消息TTL（IPv4）/HopLimit（IPv6）（默认DefaultTTL=255）
}

// NewMulticastHandler 创建组播处理器实例，初始化核心资源与连接
// 参数：config - 组播配置（nil时使用默认配置：启用IPv4/IPv6、端口5683、TTL=255）
// 返回：组播处理器实例，若初始化失败则返回错误
func NewMulticastHandler(config *MulticastConfig, netmgr *network.Manager) (*MulticastHandler, error) {
	// 若配置为nil，使用默认配置
	if config == nil {
		config = &MulticastConfig{
			IPv4Enabled: true,
			IPv6Enabled: false,
			Port:        CoAPPort,
			TTL:         DefaultTTL,
		}
	}

	// 创建可取消上下文，用于后续控制处理器停止
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化组播处理器核心字段
	handler := &MulticastHandler{
		ctx:    ctx,
		cancel: cancel,
		log:    logger.Default(),
		router: mux.NewRouter(),
	}

	// 1. 使用网络管理器设置支持组播的接口
	if err := handler.SetInterfacesFromManager(netmgr); err != nil {
		cancel() // 接口来源失败，取消上下文并释放资源
		return nil, fmt.Errorf("从网络管理器获取接口失败: %w", err)
	}

	// 2. 初始化IPv4组播连接（若启用）
	if config.IPv4Enabled {
		if err := handler.setupIPv4Multicast(config); err != nil {
			handler.log.Warn("IPv4组播连接初始化失败", zap.Error(err)) // 非致命错误，仅警告（仍可使用IPv6）
		}
	}

	// 3. 初始化IPv6组播连接（若启用）
	if config.IPv6Enabled {
		if err := handler.setupIPv6Multicast(config); err != nil {
			handler.log.Warn("IPv6组播连接初始化失败", zap.Error(err)) // 非致命错误，仅警告（仍可使用IPv4）
		}
	}

	return handler, nil
}

// setupIPv4Multicast 初始化IPv4组播连接，包括创建UDP监听、设置Socket选项、加入组播组
// 参数：config - 组播配置（含端口、TTL等参数）
// 返回：初始化失败则返回错误
func (h *MulticastHandler) setupIPv4Multicast(config *MulticastConfig) error {
	h.mu.Lock()         // 写锁：保障连接初始化时的线程安全
	defer h.mu.Unlock() // 函数退出时释放锁

	// 1. 用go-coap创建UDP监听（替代net.ListenUDP）
	addrStr := fmt.Sprintf("0.0.0.0:%d", config.Port)
	conn, err := coapNet.NewListenUDP("udp4", addrStr)
	if err != nil {
		return fmt.Errorf("创建IPv4 UDP连接失败: %w", err)
	}
	h.coapConn4 = conn
	h.udpConn4 = conn.NetConn().(*net.UDPConn) // 保留底层UDP连接用于组播配置

	// 2. 配置组播参数（加入组播组、TTL等，复用原有逻辑）
	packetConn := ipv4.NewPacketConn(h.udpConn4)
	h.ipv4Conn = packetConn
	// 设置组播TTL
	if err := packetConn.SetMulticastTTL(config.TTL); err != nil {
		h.log.Warn("设置IPv4组播TTL失败", zap.Error(err))
	}
	// 禁用IPv4组播回环（本机不接收自己发送的组播包）
	if err := packetConn.SetMulticastLoopback(false); err != nil {
		h.log.Warn("禁用IPv4组播回环失败", zap.Error(err))
	}
	// 加入组播组（原有逻辑复用）
	groupIP := net.ParseIP(MulticastIPv4Addr)
	for _, iface := range h.interfaces {
		if err := packetConn.JoinGroup(&iface, &net.UDPAddr{IP: groupIP}); err != nil {
			h.log.Warn("加入IPv4组播组失败", zap.String("接口", iface.Name), zap.Error(err))
		}
	}

	// 3. 创建go-coap服务器，绑定共享router
	h.udpServer = udp.NewServer(
		options.WithMux(h.router), // 绑定共享路由
		options.WithContext(h.ctx),
	)

	return nil
}

// setupIPv6Multicast 初始化IPv6组播连接，逻辑与IPv4类似（适配IPv6特性）
// 参数：config - 组播配置（含端口、TTL等参数）
// 返回：初始化失败则返回错误
func (h *MulticastHandler) setupIPv6Multicast(config *MulticastConfig) error {
	h.mu.Lock()         // 写锁：保障连接初始化时的线程安全
	defer h.mu.Unlock() // 函数退出时释放锁

	addrStr := fmt.Sprintf("[::]:%d", config.Port)
	conn, err := coapNet.NewListenUDP("udp6", addrStr)
	if err != nil {
		return fmt.Errorf("创建IPv6 UDP连接失败: %w", err)
	}
	h.coapConn6 = conn
	h.udpConn6 = conn.NetConn().(*net.UDPConn)

	// 包装为ipv6.PacketConn以支持组播
	packetConn := ipv6.NewPacketConn(h.udpConn6)
	h.ipv6Conn = packetConn

	// 设置组播跳数限制
	if err := packetConn.SetMulticastHopLimit(config.TTL); err != nil {
		return fmt.Errorf("设置IPv6组播跳数失败: %w", err)
	}

	// 禁用IPv6组播回环
	if err := packetConn.SetMulticastLoopback(false); err != nil {
		h.log.Warn("禁用IPv6组播回环失败", zap.Error(err))
	}

	// 加入组播组
	groupIP := net.ParseIP(MulticastIPv6Addr)
	for _, iface := range h.interfaces {
		if err := packetConn.JoinGroup(&iface, &net.UDPAddr{IP: groupIP}); err != nil {
			h.log.Warn("加入IPv6组播组失败", zap.String("iface", iface.Name), zap.Error(err))
		}
	}

	return nil
}

// Start 启动组播处理器，开启IPv4/IPv6组播消息监听协程
// 返回：启动失败则返回错误（通常是连接未初始化导致）
func (h *MulticastHandler) Start() {
	h.mu.RLock()         // 读锁：仅读取连接状态，不修改
	defer h.mu.RUnlock() // 函数退出时释放锁

	if h.udpServer != nil && h.coapConn4 != nil {
		go func() {
			if err := h.udpServer.Serve(h.coapConn4); err != nil && h.ctx.Err() == nil {
				h.log.Error("IPv4 CoAP服务器异常", zap.Error(err))
			}
		}()
	}
	if h.udpServer != nil && h.coapConn6 != nil {
		go func() {
			if err := h.udpServer.Serve(h.coapConn6); err != nil && h.ctx.Err() == nil {
				h.log.Error("IPv6 CoAP服务器异常", zap.Error(err))
			}
		}()
	}
	h.log.Info("组播处理器已启动")
}

// Stop 停止组播处理器，释放所有资源（线程安全）
func (h *MulticastHandler) Stop() {
	h.mu.Lock()         // 写锁：保障资源释放时的线程安全
	defer h.mu.Unlock() // 函数退出时释放锁

	// 1. 触发上下文取消，通知监听协程退出
	h.cancel()

	// 2. 离开IPv4组播组（避免占用组播资源）
	if h.ipv4Conn != nil {
		groupIP := net.ParseIP(MulticastIPv4Addr)
		for _, iface := range h.interfaces {
			h.ipv4Conn.LeaveGroup(&iface, &net.UDPAddr{IP: groupIP})
		}
	}

	// 3. 离开IPv6组播组
	if h.ipv6Conn != nil {
		groupIP := net.ParseIP(MulticastIPv6Addr)
		for _, iface := range h.interfaces {
			h.ipv6Conn.LeaveGroup(&iface, &net.UDPAddr{IP: groupIP})
		}
	}

	// 4. 关闭UDP连接
	if h.udpServer != nil {
		h.udpServer.Stop()
	}
	if h.udpConn4 != nil {
		h.udpConn4.Close()
	}
	if h.udpConn6 != nil {
		h.udpConn6.Close()
	}

	h.log.Info("组播处理器已停止，资源已释放")
}

// GetRouter 暴露路由供外部注入
func (h *MulticastHandler) GetRouter() *mux.Router {
	return h.router
}

// SendMulticast 发送组播消息到所有支持的网络接口（IPv4/IPv6双栈）
// 参数：data - 待发送的消息数据（通常是CoAP编码后的字节流）
// 返回：所有接口发送失败则返回错误，部分成功则返回nil
func (h *MulticastHandler) SendMulticast(data []byte) error {
	h.mu.RLock()         // 读锁：仅读取连接和接口状态，不修改
	defer h.mu.RUnlock() // 函数退出时释放锁

	var lastErr error // 记录最后一次发送错误（用于最终返回）
	sentCount := 0    // 记录成功发送的接口数量

	// 1. 通过IPv4发送组播消息（若IPv4连接已初始化）
	if h.ipv4Conn != nil {
		// 解析IPv4组播目标地址（组播IP:端口）
		dstAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", MulticastIPv4Addr, CoAPPort))
		// 遍历所有支持的接口，逐个发送
		for _, iface := range h.interfaces {
			// 设置当前发送使用的网络接口
			if err := h.ipv4Conn.SetMulticastInterface(&iface); err != nil {
				h.log.Warn("设置IPv4组播发送接口失败",
					zap.String("接口名称", iface.Name),
					zap.Error(err))
				continue
			}

			// 发送消息到目标组播地址
			if _, err := h.udpConn4.WriteTo(data, dstAddr); err != nil {
				lastErr = err // 更新最后一次错误
				h.log.Warn("IPv4组播消息发送失败",
					zap.String("接口名称", iface.Name),
					zap.Error(err))
			} else {
				sentCount++ // 成功发送计数+1
				h.log.Debug("IPv4组播消息发送成功",
					zap.String("接口名称", iface.Name),
					zap.Int("消息大小", len(data)))
			}
		}
	}

	// 2. 通过IPv6发送组播消息（若IPv6连接已初始化）
	if h.ipv6Conn != nil {
		// 解析IPv6组播目标地址（[组播IP]:端口）
		dstAddr, _ := net.ResolveUDPAddr("udp6", fmt.Sprintf("[%s]:%d", MulticastIPv6Addr, CoAPPort))
		// 遍历所有支持的接口，逐个发送
		for _, iface := range h.interfaces {
			// 仅在接口具备IPv6地址时发送，避免虚拟接口/不支持IPv6导致的不可达错误
			hasIPv6 := false
			if addrs, err := iface.Addrs(); err == nil {
				for _, a := range addrs {
					switch v := a.(type) {
					case *net.IPNet:
						if v.IP.To4() == nil {
							hasIPv6 = true
						}
					case *net.IPAddr:
						if v.IP.To4() == nil {
							hasIPv6 = true
						}
					}
					if hasIPv6 {
						break
					}
				}
			}
			if !hasIPv6 {
				h.log.Debug("跳过不具备IPv6地址的接口",
					zap.String("接口名称", iface.Name))
				continue
			}

			if err := h.ipv6Conn.SetMulticastInterface(&iface); err != nil {
				h.log.Warn("设置IPv6组播发送接口失败",
					zap.String("接口名称", iface.Name),
					zap.Error(err))
				continue
			}

			if _, err := h.udpConn6.WriteTo(data, dstAddr); err != nil {
				lastErr = err
				h.log.Warn("IPv6组播消息发送失败",
					zap.String("接口名称", iface.Name),
					zap.Error(err))
			} else {
				sentCount++
				h.log.Debug("IPv6组播消息发送成功",
					zap.String("接口名称", iface.Name),
					zap.Int("消息大小", len(data)))
			}
		}
	}

	// 若所有接口都发送失败，返回最后一次错误
	if sentCount == 0 && lastErr != nil {
		return fmt.Errorf("所有接口组播消息发送失败: %w", lastErr)
	}

	return nil
}

// SendMulticastToInterface 发送组播消息到指定的网络接口（精准发送）
// 参数：data - 待发送的消息数据；ifaceName - 目标接口名称
// 返回：接口不存在或发送失败则返回错误
func (h *MulticastHandler) SendMulticastToInterface(data []byte, ifaceName string) error {
	h.mu.RLock()         // 读锁：仅读取连接和接口状态
	defer h.mu.RUnlock() // 函数退出时释放锁

	// 1. 查找指定名称的网络接口
	var targetIface *net.Interface
	for _, iface := range h.interfaces {
		if iface.Name == ifaceName {
			targetIface = &iface
			break
		}
	}
	// 若接口不存在，返回错误
	if targetIface == nil {
		return fmt.Errorf("指定的网络接口不存在: %s", ifaceName)
	}

	sent := false // 标记是否发送成功

	// 2. 通过IPv4发送（若IPv4连接已初始化）
	if h.ipv4Conn != nil {
		dstAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", MulticastIPv4Addr, CoAPPort))
		// 设置发送接口并发送
		if err := h.ipv4Conn.SetMulticastInterface(targetIface); err == nil {
			if _, err := h.udpConn4.WriteTo(data, dstAddr); err == nil {
				sent = true
			}
		}
	}

	// 3. 通过IPv6发送（若IPv6连接已初始化，且IPv4未发送成功）
	if h.ipv6Conn != nil && !sent {
		dstAddr, _ := net.ResolveUDPAddr("udp6", fmt.Sprintf("[%s]:%d", MulticastIPv6Addr, CoAPPort))
		if err := h.ipv6Conn.SetMulticastInterface(targetIface); err == nil {
			if _, err := h.udpConn6.WriteTo(data, dstAddr); err == nil {
				sent = true
			}
		}
	}

	// 若IPv4和IPv6都发送失败，返回错误
	if !sent {
		return fmt.Errorf("在接口%s上发送组播消息失败", ifaceName)
	}

	return nil
}

// GetInterfaces 获取当前支持组播的网络接口列表（返回副本，避免外部修改）
// 返回：网络接口列表副本
func (h *MulticastHandler) GetInterfaces() []net.Interface {
	h.mu.RLock()         // 读锁：保障读取时列表不被修改
	defer h.mu.RUnlock() // 函数退出时释放锁

	// 创建列表副本并返回（避免外部直接修改handler.interfaces）
	interfaces := make([]net.Interface, len(h.interfaces))
	copy(interfaces, h.interfaces)
	return interfaces
}

// SetInterfacesFromManager 使用 network.Manager 提供的接口列表，统一设置支持组播的接口
// 通过接口名称查找系统 net.Interface，替换处理器的接口列表
func (h *MulticastHandler) SetInterfacesFromManager(netmgr *network.Manager) error {
	if netmgr == nil {
		return fmt.Errorf("未提供网络管理器")
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	h.netmgr = netmgr

	infos := netmgr.GetMulticastInterfaces()
	if len(infos) == 0 {
		return fmt.Errorf("网络管理器未提供可用的组播接口")
	}

	list := make([]net.Interface, 0, len(infos))
	for _, info := range infos {
		if info.Name == "" {
			continue
		}
		iface, err := net.InterfaceByName(info.Name)
		if err != nil {
			h.log.Warn("通过名称查找接口失败",
				zap.String("接口名称", info.Name),
				zap.Error(err))
			continue
		}
		list = append(list, *iface)
		h.log.Debug("添加支持组播的网络接口(来自network.Manager)",
			zap.String("接口名称", iface.Name),
			zap.Int("接口索引", iface.Index))
	}

	if len(list) == 0 {
		return fmt.Errorf("无法从网络管理器解析任何有效接口")
	}

	h.interfaces = list
	return nil
}

// SendUnicastIPv6 发送 IPv6 单播消息，支持链路本地地址的 zone
// 参数：remoteIP - 目标 IPv6 地址；zone - 接收接口名（链路本地必须）；payload - 要发送的数据
func (h *MulticastHandler) SendUnicastIPv6(remoteIP net.IP, zone string, payload []byte) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	dst := &net.UDPAddr{
		IP:   remoteIP,
		Port: CoAPPort,
		Zone: zone,
	}

	// 优先使用已初始化的 udpConn6；否则回退 DialUDP
	var conn *net.UDPConn
	var err error
	if h.udpConn6 != nil {
		conn = h.udpConn6
	} else {
		conn, err = net.DialUDP("udp6", nil, dst)
		if err != nil {
			return fmt.Errorf("连接到IPv6地址失败: %w", err)
		}
		defer conn.Close()
	}

	// 设置写超时并尝试发送（一次重试）
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.WriteToUDP(payload, dst); err != nil {
		// 重试一次
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err2 := conn.WriteToUDP(payload, dst); err2 != nil {
			return fmt.Errorf("发送IPv6单播消息失败: %w", err2)
		}
	}
	return nil
}

// SendUnicastIPv4 发送 IPv4 单播原始字节（端口使用 CoAP 默认端口）
func (h *MulticastHandler) SendUnicastIPv4(remoteIP net.IP, payload []byte) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if remoteIP == nil || remoteIP.To4() == nil {
		return fmt.Errorf("无效的IPv4地址")
	}
	dst := &net.UDPAddr{
		IP:   remoteIP,
		Port: CoAPPort,
	}

	// 优先使用已初始化的 udpConn4；否则回退 DialUDP
	var conn *net.UDPConn
	var err error
	if h.udpConn4 != nil {
		conn = h.udpConn4
	} else {
		conn, err = net.DialUDP("udp4", nil, dst)
		if err != nil {
			return fmt.Errorf("连接到IPv4地址失败: %w", err)
		}
		defer conn.Close()
	}

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.WriteToUDP(payload, dst); err != nil {
		// 重试一次
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err2 := conn.WriteToUDP(payload, dst); err2 != nil {
			return fmt.Errorf("发送IPv4单播消息失败: %w", err2)
		}
	}
	return nil
}
