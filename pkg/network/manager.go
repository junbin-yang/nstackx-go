// 提供网络接口管理功能，包括接口信息获取、状态监控等
package network

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// InterfaceInfo 表示网络接口的详细信息
type InterfaceInfo struct {
	Name      string           // 接口名称（如eth0、lo等）
	Index     int              // 接口索引（系统分配的唯一标识）
	Flags     net.Flags        // 接口标志（如是否启用、是否为回环等）
	Addresses []net.IP         // 接口关联的IP地址列表
	MAC       net.HardwareAddr // 接口的MAC地址
	MTU       int              // 接口的最大传输单元(MTU)
}

// Manager 用于管理网络接口，提供接口信息的扫描、监控和查询功能
type Manager struct {
	mu sync.RWMutex // 读写锁，保证并发安全

	interfaces map[string]*InterfaceInfo // 存储接口信息，键为接口名称
	monitoring bool                      // 标识是否正在监控接口
	stopChan   chan struct{}             // 用于停止监控循环的通道
	log        *logger.Logger            // 日志记录器
}

// NewManager 创建一个新的网络接口管理器
func NewManager() (*Manager, error) {
	m := &Manager{
		interfaces: make(map[string]*InterfaceInfo),
		stopChan:   make(chan struct{}),
		log:        logger.Default(),
	}

	// 初始化扫描接口信息
	if err := m.scanInterfaces(); err != nil {
		return nil, fmt.Errorf("扫描接口失败: %w", err)
	}

	return m, nil
}

// Start 开始监控网络接口（定期扫描更新接口信息）
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.monitoring {
		return fmt.Errorf("已在监控中")
	}

	m.monitoring = true
	go m.monitorLoop() // 启动监控循环协程

	m.log.Info("网络监控已启动")
	return nil
}

// Stop 停止监控网络接口
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.monitoring {
		return
	}

	m.monitoring = false
	close(m.stopChan) // 关闭通道以终止监控循环

	m.log.Info("网络监控已停止")
}

// GetInterfaces 返回所有网络接口的信息
func (m *Manager) GetInterfaces() []InterfaceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	interfaces := make([]InterfaceInfo, 0, len(m.interfaces))
	for _, iface := range m.interfaces {
		interfaces = append(interfaces, *iface)
	}

	return interfaces
}

// GetInterface 通过名称获取特定网络接口的信息
func (m *Manager) GetInterface(name string) (*InterfaceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	iface, exists := m.interfaces[name]
	if !exists {
		return nil, fmt.Errorf("未找到接口 %s", name)
	}

	// 返回副本以避免外部修改内部数据
	copy := *iface
	return &copy, nil
}

// GetActiveInterfaces 返回所有活跃的网络接口（已启用且有IP地址）
func (m *Manager) GetActiveInterfaces() []InterfaceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	active := make([]InterfaceInfo, 0)
	for _, iface := range m.interfaces {
		// 检查接口是否启用且有IP地址
		if iface.Flags&net.FlagUp != 0 && len(iface.Addresses) > 0 {
			active = append(active, *iface)
		}
	}

	return active
}

// GetMulticastInterfaces 返回适合多播的网络接口
func (m *Manager) GetMulticastInterfaces() []InterfaceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	multicast := make([]InterfaceInfo, 0)
	for _, iface := range m.interfaces {
		// 检查接口是否启用、支持多播、非回环且有IP地址
		if iface.Flags&net.FlagUp != 0 &&
			iface.Flags&net.FlagMulticast != 0 &&
			iface.Flags&net.FlagLoopback == 0 &&
			len(iface.Addresses) > 0 {
			multicast = append(multicast, *iface)
		}
	}

	return multicast
}

// scanInterfaces 扫描并更新所有网络接口的信息
func (m *Manager) scanInterfaces() error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("获取接口列表失败: %w", err)
	}

	newInterfaces := make(map[string]*InterfaceInfo)

	for _, iface := range interfaces {
		info := &InterfaceInfo{
			Name:  iface.Name,
			Index: iface.Index,
			Flags: iface.Flags,
			MAC:   iface.HardwareAddr,
			MTU:   iface.MTU,
		}

		// 获取接口关联的地址
		addrs, err := iface.Addrs()
		if err != nil {
			m.log.Warn("获取接口地址失败",
				zap.String("interface", iface.Name),
				zap.Error(err))
			continue
		}

		info.Addresses = make([]net.IP, 0)
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				info.Addresses = append(info.Addresses, v.IP)
			case *net.IPAddr:
				info.Addresses = append(info.Addresses, v.IP)
			}
		}

		newInterfaces[iface.Name] = info
	}

	m.mu.Lock()
	m.interfaces = newInterfaces
	m.mu.Unlock()

	m.log.Debug("已扫描接口",
		zap.Int("count", len(newInterfaces)))

	return nil
}

// monitorLoop 持续监控网络接口（定期扫描更新）
func (m *Manager) monitorLoop() {
	ticker := time.NewTicker(5 * time.Second) // 每5秒扫描一次
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan: // 收到停止信号时退出
			return
		case <-ticker.C: // 定时触发扫描
			if err := m.scanInterfaces(); err != nil {
				m.log.Error("扫描接口失败", zap.Error(err))
			}
		}
	}
}

// GetDefaultInterface 返回默认网络接口（简化实现）
func (m *Manager) GetDefaultInterface() (*InterfaceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 尝试找到默认路由对应的接口（简化版：优先找启用的非回环IPv4接口）
	for _, iface := range m.interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			for _, addr := range iface.Addresses {
				if addr.To4() != nil && !addr.IsLoopback() {
					copy := *iface
					return &copy, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("未找到默认接口")
}

// IsInterfaceUp 检查指定名称的接口是否处于启用状态
func (m *Manager) IsInterfaceUp(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	iface, exists := m.interfaces[name]
	if !exists {
		return false
	}

	// 检查接口标志中是否包含"启用"标志
	return iface.Flags&net.FlagUp != 0
}

// GetIPv4Addresses 返回所有启用接口的非回环IPv4地址
func (m *Manager) GetIPv4Addresses() []net.IP {
	m.mu.RLock()
	defer m.mu.RUnlock()

	addresses := make([]net.IP, 0)
	for _, iface := range m.interfaces {
		if iface.Flags&net.FlagUp != 0 {
			for _, addr := range iface.Addresses {
				if ipv4 := addr.To4(); ipv4 != nil && !ipv4.IsLoopback() {
					addresses = append(addresses, ipv4)
				}
			}
		}
	}

	return addresses
}

// GetIPv6Addresses 返回所有启用接口的非回环IPv6地址
func (m *Manager) GetIPv6Addresses() []net.IP {
	m.mu.RLock()
	defer m.mu.RUnlock()

	addresses := make([]net.IP, 0)
	for _, iface := range m.interfaces {
		if iface.Flags&net.FlagUp != 0 {
			for _, addr := range iface.Addresses {
				// IPv6地址的To4()返回nil
				if addr.To4() == nil && !addr.IsLoopback() {
					addresses = append(addresses, addr)
				}
			}
		}
	}

	return addresses
}

// GetInterfaceMTU 获取指定接口的MTU
func (m *Manager) GetInterfaceMTU(name string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	iface, exists := m.interfaces[name]
	if !exists {
		return 0, fmt.Errorf("interface %s not found", name)
	}

	return iface.MTU, nil
}

// IsLocalIP 判断IP是否为本地IP
func (m *Manager) IsLocalIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true // 回环地址（127.0.0.1/::1）
	}

	// 获取本机所有网络接口的IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}

	for _, addr := range addrs {
		// 解析IP地址（忽略端口）
		var localIP net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			localIP = v.IP
		case *net.IPAddr:
			localIP = v.IP
		default:
			continue
		}

		// 比较IP（忽略IPv4/IPv6差异）
		if ip.Equal(localIP) {
			return true
		}
	}
	return false
}
