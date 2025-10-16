// CoAP协议的组播通信支持，核心用于设备发现场景
package coap

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	// CoAP组播地址常量（符合RFC 7252标准）
	MulticastIPv4Addr = "224.0.1.187" // IPv4站点本地组播地址（CoAP标准设备发现地址）
	MulticastIPv6Addr = "ff02::1"     // IPv6链路本地“所有节点”组播地址
	CoAPPort          = 5683          // CoAP协议默认端口（UDP）

	// 备选CoAP组播地址（用于不同场景）
	AllCoAPNodesIPv4 = "224.0.1.187" // 同MulticastIPv4Addr，语义为“所有CoAP节点”
	AllCoAPNodesIPv6 = "ff02::fd"    // IPv6链路本地“所有CoAP节点”组播地址（更精准的CoAP专用）
	SiteLocalIPv6    = "ff05::fd"    // IPv6站点本地“所有CoAP节点”组播地址

	// 网络参数常量
	DefaultTTL      = 255   // IPv4组播默认TTL（生存时间，控制消息传播范围）
	DefaultHopLimit = 255   // IPv6组播默认跳数限制（同TTL作用）
	MaxDatagramSize = 1500  // 最大UDP数据报大小（适配MTU=1500的常规网络）
	ReadBufferSize  = 65536 // UDP读取缓冲区大小（64KB，避免小缓冲区导致消息丢失）
)

// MulticastHandler 组播通信处理器，管理组播连接、接口、消息队列等核心资源
type MulticastHandler struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问时的资源安全

	ipv4Conn     *ipv4.PacketConn       // IPv4组播数据包连接（基于x/net/ipv4封装）
	ipv6Conn     *ipv6.PacketConn       // IPv6组播数据包连接（基于x/net/ipv6封装）
	udpConn4     *net.UDPConn           // 原始IPv4 UDP连接（供ipv4Conn底层使用）
	udpConn6     *net.UDPConn           // 原始IPv6 UDP连接（供ipv6Conn底层使用）
	interfaces   []net.Interface        // 支持组播的网络接口列表（已筛选）
	messageQueue chan *MulticastMessage // 组播消息接收队列（缓冲100条，避免阻塞）
	ctx          context.Context        // 上下文，用于协程退出控制
	cancel       context.CancelFunc     // 上下文取消函数，触发处理器停止
	log          *logger.Logger         // 日志实例（基于zap框架）
}

// MulticastMessage 组播消息结构体，封装接收的组播消息元数据
type MulticastMessage struct {
	Data      []byte        // 消息原始字节数据（通常是CoAP编码后的数据包）
	Source    net.Addr      // 消息来源地址（发送方的IP:Port）
	Interface net.Interface // 接收消息的网络接口（明确消息来自哪个网卡）
	Timestamp time.Time     // 消息接收时间戳（用于时序处理）
}

// MulticastConfig 组播配置结构体，用于初始化MulticastHandler时自定义参数
type MulticastConfig struct {
	IPv4Enabled     bool     // 是否启用IPv4组播
	IPv6Enabled     bool     // 是否启用IPv6组播
	Interfaces      []string // 指定使用的网络接口名称（空则自动筛选所有符合条件的接口）
	Port            int      // 组播监听/发送端口（默认使用CoAPPort=5683）
	TTL             int      // 组播消息TTL（IPv4）/HopLimit（IPv6）（默认DefaultTTL=255）
	LoopbackEnabled bool     // 是否允许组播消息回环（即自己发送的消息自己能收到，默认未指定）
}

// NewMulticastHandler 创建组播处理器实例，初始化核心资源与连接
// 参数：config - 组播配置（nil时使用默认配置：启用IPv4/IPv6、端口5683、TTL=255）
// 返回：组播处理器实例，若初始化失败则返回错误
func NewMulticastHandler(config *MulticastConfig) (*MulticastHandler, error) {
	// 若配置为nil，使用默认配置
	if config == nil {
		config = &MulticastConfig{
			IPv4Enabled: true,
			IPv6Enabled: true,
			Port:        CoAPPort,
			TTL:         DefaultTTL,
		}
	}

	// 创建可取消上下文，用于后续控制处理器停止
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化组播处理器核心字段
	handler := &MulticastHandler{
		messageQueue: make(chan *MulticastMessage, 100), // 缓冲100条消息的队列
		ctx:          ctx,
		cancel:       cancel,
		log:          logger.Default(),
	}

	// 1. 筛选符合条件的网络接口
	if err := handler.discoverInterfaces(config.Interfaces); err != nil {
		cancel() // 接口筛选失败，取消上下文并释放资源
		return nil, fmt.Errorf("筛选网络接口失败: %w", err)
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

// discoverInterfaces 筛选符合条件的网络接口，存入handler.interfaces
// 参数：specific - 指定的接口名称列表（空则筛选所有符合条件的接口）
// 返回：筛选失败则返回错误
func (h *MulticastHandler) discoverInterfaces(specific []string) error {
	// 获取系统所有网络接口
	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	h.interfaces = make([]net.Interface, 0)

	// 遍历所有接口，筛选符合组播要求的接口
	for _, iface := range interfaces {
		// 1. 排除未启用的接口（FlagUp表示接口已启用）
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		// 2. 排除回环接口（回环接口仅本地通信，不支持跨设备组播）
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// 3. 排除不支持组播的接口（FlagMulticast表示接口支持组播）
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}

		// 4. 若指定了接口列表，仅保留在列表中的接口
		if len(specific) > 0 {
			found := false
			for _, name := range specific {
				if iface.Name == name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// 符合条件的接口加入列表
		h.interfaces = append(h.interfaces, iface)
		h.log.Debug("添加支持组播的网络接口",
			zap.String("接口名称", iface.Name),
			zap.Int("接口索引", iface.Index))
	}

	// 若没有符合条件的接口，返回错误（无法进行组播通信）
	if len(h.interfaces) == 0 {
		return fmt.Errorf("未找到支持组播的网络接口")
	}

	return nil
}

// setupIPv4Multicast 初始化IPv4组播连接，包括创建UDP监听、设置Socket选项、加入组播组
// 参数：config - 组播配置（含端口、TTL等参数）
// 返回：初始化失败则返回错误
func (h *MulticastHandler) setupIPv4Multicast(config *MulticastConfig) error {
	h.mu.Lock()         // 写锁：保障连接初始化时的线程安全
	defer h.mu.Unlock() // 函数退出时释放锁

	// 1. 解析IPv4组播监听地址（0.0.0.0:端口，监听所有网卡的该端口）
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", config.Port))
	if err != nil {
		return err
	}

	// 2. 创建IPv4 UDP监听连接（绑定到指定端口）
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}

	// 3. 设置UDP读取缓冲区大小（避免小缓冲区导致消息丢失）
	if err := conn.SetReadBuffer(ReadBufferSize); err != nil {
		h.log.Warn("设置IPv4 UDP读取缓冲区失败", zap.Error(err)) // 非致命错误，仅警告
	}

	// 4. 封装为ipv4.PacketConn（提供组播专用API，如JoinGroup、SetMulticastTTL）
	h.udpConn4 = conn
	h.ipv4Conn = ipv4.NewPacketConn(conn)

	// 5. 设置IPv4组播TTL（控制消息传播范围，255表示最大范围）
	if err := h.ipv4Conn.SetMulticastTTL(config.TTL); err != nil {
		h.log.Warn("设置IPv4组播TTL失败", zap.Error(err))
	}

	// 6. 设置IPv4组播回环（是否允许自己发送的消息自己接收，按需配置）
	if err := h.ipv4Conn.SetMulticastLoopback(config.LoopbackEnabled); err != nil {
		h.log.Warn("设置IPv4组播回环失败", zap.Error(err))
	}

	// 7. 在每个支持的接口上加入IPv4组播组（只有加入组播组才能接收该组的消息）
	groupIP := net.ParseIP(MulticastIPv4Addr) // 解析组播组IP
	for _, iface := range h.interfaces {
		// 加入组播组：指定接口和组播地址
		if err := h.ipv4Conn.JoinGroup(&iface, &net.UDPAddr{IP: groupIP}); err != nil {
			h.log.Warn("加入IPv4组播组失败",
				zap.String("接口名称", iface.Name),
				zap.Error(err))
			continue // 某个接口失败不影响其他接口
		}
		h.log.Info("成功加入IPv4组播组",
			zap.String("接口名称", iface.Name),
			zap.String("组播地址", MulticastIPv4Addr))
	}

	return nil
}

// setupIPv6Multicast 初始化IPv6组播连接，逻辑与IPv4类似（适配IPv6特性）
// 参数：config - 组播配置（含端口、TTL等参数）
// 返回：初始化失败则返回错误
func (h *MulticastHandler) setupIPv6Multicast(config *MulticastConfig) error {
	h.mu.Lock()         // 写锁：保障连接初始化时的线程安全
	defer h.mu.Unlock() // 函数退出时释放锁

	// 1. 解析IPv6组播监听地址（[::]:端口，监听所有网卡的该端口）
	addr, err := net.ResolveUDPAddr("udp6", fmt.Sprintf("[::]:%d", config.Port))
	if err != nil {
		return err
	}

	// 2. 创建IPv6 UDP监听连接（绑定到指定端口）
	conn, err := net.ListenUDP("udp6", addr)
	if err != nil {
		return err
	}

	// 3. 设置UDP读取缓冲区大小
	if err := conn.SetReadBuffer(ReadBufferSize); err != nil {
		h.log.Warn("设置IPv6 UDP读取缓冲区失败", zap.Error(err))
	}

	// 4. 封装为ipv6.PacketConn（IPv6组播专用API）
	h.udpConn6 = conn
	h.ipv6Conn = ipv6.NewPacketConn(conn)

	// 5. 设置IPv6组播跳数限制（作用同IPv4的TTL，控制消息传播范围）
	if err := h.ipv6Conn.SetMulticastHopLimit(config.TTL); err != nil {
		h.log.Warn("设置IPv6组播跳数限制失败", zap.Error(err))
	}

	// 6. 设置IPv6组播回环
	if err := h.ipv6Conn.SetMulticastLoopback(config.LoopbackEnabled); err != nil {
		h.log.Warn("设置IPv6组播回环失败", zap.Error(err))
	}

	// 7. 在每个支持的接口上加入IPv6组播组
	groupIP := net.ParseIP(MulticastIPv6Addr)
	for _, iface := range h.interfaces {
		if err := h.ipv6Conn.JoinGroup(&iface, &net.UDPAddr{IP: groupIP}); err != nil {
			h.log.Warn("加入IPv6组播组失败",
				zap.String("接口名称", iface.Name),
				zap.Error(err))
			continue
		}
		h.log.Info("成功加入IPv6组播组",
			zap.String("接口名称", iface.Name),
			zap.String("组播地址", MulticastIPv6Addr))
	}

	return nil
}

// Start 启动组播处理器，开启IPv4/IPv6组播消息监听协程
// 返回：启动失败则返回错误（通常是连接未初始化导致）
func (h *MulticastHandler) Start() error {
	h.mu.RLock()         // 读锁：仅读取连接状态，不修改
	defer h.mu.RUnlock() // 函数退出时释放锁

	// 启动IPv4组播消息监听协程（若IPv4连接已初始化）
	if h.udpConn4 != nil {
		go h.listenIPv4() // 协程内循环读取消息，不阻塞当前函数
	}

	// 启动IPv6组播消息监听协程（若IPv6连接已初始化）
	if h.udpConn6 != nil {
		go h.listenIPv6()
	}

	h.log.Info("组播处理器已启动")
	return nil
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
	if h.udpConn4 != nil {
		h.udpConn4.Close()
	}
	if h.udpConn6 != nil {
		h.udpConn6.Close()
	}

	// 5. 关闭消息队列（避免消费者协程阻塞）
	close(h.messageQueue)
	h.log.Info("组播处理器已停止，资源已释放")
}

// listenIPv4 监听IPv4组播消息，循环读取并写入消息队列（独立协程运行）
func (h *MulticastHandler) listenIPv4() {
	buffer := make([]byte, MaxDatagramSize) // 消息读取缓冲区（适配最大数据报大小）

	for {
		select {
		// 若上下文已取消（处理器停止），退出循环
		case <-h.ctx.Done():
			return
		default:
			// 设置读取超时（1秒），避免ReadFrom永久阻塞
			h.udpConn4.SetReadDeadline(time.Now().Add(1 * time.Second))
			// 读取IPv4组播消息：n=读取字节数，cm=控制消息（含接口索引），src=来源地址
			n, cm, src, err := h.ipv4Conn.ReadFrom(buffer)
			if err != nil {
				// 若为超时错误，继续循环（仅超时，非致命）
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				// 若上下文未取消（非主动停止），打印错误日志
				if h.ctx.Err() == nil {
					h.log.Error("IPv4组播消息读取错误", zap.Error(err))
				}
				continue
			}

			// 根据控制消息中的接口索引，获取对应的网络接口
			var iface net.Interface
			if cm != nil && cm.IfIndex > 0 {
				if i, err := net.InterfaceByIndex(cm.IfIndex); err == nil {
					iface = *i
				}
			}

			// 封装组播消息（复制缓冲区有效数据，避免后续覆盖）
			msg := &MulticastMessage{
				Data:      buffer[:n], // 仅保留读取到的有效数据
				Source:    src,        // 消息来源
				Interface: iface,      // 接收接口
				Timestamp: time.Now(), // 接收时间戳
			}

			// 将消息写入队列（非阻塞，队列满时丢弃并警告）
			select {
			case h.messageQueue <- msg:
			case <-h.ctx.Done():
				return
			default:
				h.log.Warn("组播消息队列已满，丢弃当前消息")
			}
		}
	}
}

// listenIPv6 监听IPv6组播消息，逻辑与listenIPv4一致（适配IPv6连接）
func (h *MulticastHandler) listenIPv6() {
	buffer := make([]byte, MaxDatagramSize)

	for {
		select {
		case <-h.ctx.Done():
			return
		default:
			h.udpConn6.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, cm, src, err := h.ipv6Conn.ReadFrom(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if h.ctx.Err() == nil {
					h.log.Error("IPv6组播消息读取错误", zap.Error(err))
				}
				continue
			}

			// 获取接收消息的网络接口
			var iface net.Interface
			if cm != nil && cm.IfIndex > 0 {
				if i, err := net.InterfaceByIndex(cm.IfIndex); err == nil {
					iface = *i
				}
			}

			// 封装消息并写入队列
			msg := &MulticastMessage{
				Data:      buffer[:n],
				Source:    src,
				Interface: iface,
				Timestamp: time.Now(),
			}

			select {
			case h.messageQueue <- msg:
			case <-h.ctx.Done():
				return
			default:
				h.log.Warn("组播消息队列已满，丢弃当前消息")
			}
		}
	}
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

// GetMessageChannel 获取组播消息接收队列的只读通道
// 返回：只读通道，外部通过该通道消费接收的组播消息
func (h *MulticastHandler) GetMessageChannel() <-chan *MulticastMessage {
	return h.messageQueue
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
