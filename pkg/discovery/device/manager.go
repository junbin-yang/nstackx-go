// 提供设备管理功能，包括设备的添加、更新、查询、删除及过期清理等
package device

import (
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
)

// Manager 设备管理器，用于维护设备列表并处理设备的生命周期
type Manager struct {
	mu sync.RWMutex // 读写锁，保证多协程访问时的数据安全

	devices     map[string]*api.DeviceInfo // 存储设备的映射表，键为设备唯一标识DeviceID
	maxDevices  uint32                    // 最大设备数量限制，超过时会移除最旧设备
	agingTime   time.Duration             // 设备老化时间，超过此时长未更新的设备会被清理
	deviceOrder []string                  // 维护设备插入顺序的切片，用于在设备满时移除最旧设备
}

// NewManager 创建一个新的设备管理器
// 参数maxDevices：设备最大数量限制（0则使用默认值20）
// 参数agingTime：设备老化时间（0则使用默认值60秒）
func NewManager(maxDevices uint32, agingTime time.Duration) *Manager {
	if maxDevices == 0 {
		maxDevices = 20 // 默认最大设备数为20
	}
	if agingTime == 0 {
		agingTime = 60 * time.Second // 默认老化时间为60秒
	}

	return &Manager{
		devices:     make(map[string]*api.DeviceInfo),
		maxDevices:  maxDevices,
		agingTime:   agingTime,
		deviceOrder: make([]string, 0),
	}
}

// UpdateDevice 添加或更新设备列表中的设备
// 参数device：要更新的设备信息
// 返回值：true表示是新添加的设备，false表示是更新已有设备
func (m *Manager) UpdateDevice(device *api.DeviceInfo) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if device == nil {
		return false
	}

	// 更新设备最后出现时间为当前时间
	device.LastSeenTime = time.Now()

	// 检查设备是否已存在，存在则更新
	if _, exists := m.devices[device.DeviceID]; exists {
		m.devices[device.DeviceID] = device
		return false
	}

	// 若设备数量已达上限，移除最旧的设备（按插入顺序）
	if uint32(len(m.devices)) >= m.maxDevices {
		if len(m.deviceOrder) > 0 {
			oldestID := m.deviceOrder[0]
			delete(m.devices, oldestID)       // 从映射表中删除
			m.deviceOrder = m.deviceOrder[1:] // 从顺序切片中移除
		}
	}

	// 添加新设备
	m.devices[device.DeviceID] = device
	m.deviceOrder = append(m.deviceOrder, device.DeviceID) // 记录插入顺序

	return true
}

// GetDevice 通过设备ID查询设备信息
// 参数deviceID：设备唯一标识
// 返回值：设备信息（不存在则返回nil），返回副本以避免并发修改冲突
func (m *Manager) GetDevice(deviceID string) *api.DeviceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	device, exists := m.devices[deviceID]
	if !exists {
		return nil
	}

	// 返回副本，防止外部修改内部数据
	deviceCopy := *device
	return &deviceCopy
}

// GetAllDevices 返回当前所有设备的列表
// 返回值：设备信息切片（每个元素为副本，避免并发修改冲突）
func (m *Manager) GetAllDevices() []api.DeviceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	devices := make([]api.DeviceInfo, 0, len(m.devices))
	for _, device := range m.devices {
		devices = append(devices, *device) // 存储副本
	}

	return devices
}

// RemoveDevice 从列表中移除指定设备
// 参数deviceID：设备唯一标识
// 返回值：true表示成功移除，false表示设备不存在
func (m *Manager) RemoveDevice(deviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.devices[deviceID]; !exists {
		return false
	}

	// 从映射表中删除
	delete(m.devices, deviceID)

	// 从顺序切片中删除
	for i, id := range m.deviceOrder {
		if id == deviceID {
			// 拼接切片，移除当前元素
			m.deviceOrder = append(m.deviceOrder[:i], m.deviceOrder[i+1:]...)
			break
		}
	}

	return true
}

// CleanupExpired 清理过期设备（超过老化时间未更新的设备）
// 返回值：被清理的设备ID列表
func (m *Manager) CleanupExpired() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	expired := make([]string, 0)

	// 筛选过期设备
	for deviceID, device := range m.devices {
		if now.Sub(device.LastSeenTime) > m.agingTime {
			expired = append(expired, deviceID)
			delete(m.devices, deviceID)
		}
	}

	// 更新顺序切片（移除已清理的设备）
	if len(expired) > 0 {
		newOrder := make([]string, 0, len(m.deviceOrder))
		for _, id := range m.deviceOrder {
			if _, exists := m.devices[id]; exists { // 只保留仍存在的设备ID
				newOrder = append(newOrder, id)
			}
		}
		m.deviceOrder = newOrder
	}

	return expired
}

// SetAgingTime 更新设备老化时间
// 参数agingTime：新的老化时间
func (m *Manager) SetAgingTime(agingTime time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.agingTime = agingTime
}

// SetMaxDevices 更新最大设备数量限制
// 参数maxDevices：新的最大设备数量，若当前设备数超过新限制，会移除最旧的设备
func (m *Manager) SetMaxDevices(maxDevices uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.maxDevices = maxDevices

	// 若当前设备数超过新限制，移除多余的最旧设备
	if uint32(len(m.devices)) > m.maxDevices {
		removeCount := uint32(len(m.devices)) - m.maxDevices
		for i := uint32(0); i < removeCount; i++ {
			if len(m.deviceOrder) > 0 {
				oldestID := m.deviceOrder[0]
				delete(m.devices, oldestID)
				m.deviceOrder = m.deviceOrder[1:]
			}
		}
	}
}

// GetDeviceCount 返回当前设备数量
func (m *Manager) GetDeviceCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.devices)
}

// Clear 清空所有设备
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.devices = make(map[string]*api.DeviceInfo)
	m.deviceOrder = make([]string, 0)
}

// FilterDevices 根据过滤函数筛选设备
// 参数filter：设备过滤函数（返回true表示保留该设备）
// 返回值：符合条件的设备列表（每个元素为副本）
func (m *Manager) FilterDevices(filter func(*api.DeviceInfo) bool) []api.DeviceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	filtered := make([]api.DeviceInfo, 0)
	for _, device := range m.devices {
		if filter(device) {
			filtered = append(filtered, *device) // 存储副本
		}
	}

	return filtered
}

// UpdateDeviceField 更新指定设备的特定字段
// 参数deviceID：设备唯一标识
// 参数updater：字段更新函数（接收设备指针，在函数内修改字段）
// 返回值：true表示更新成功（设备存在），false表示设备不存在
func (m *Manager) UpdateDeviceField(deviceID string, updater func(*api.DeviceInfo)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	device, exists := m.devices[deviceID]
	if !exists {
		return false
	}

	// 调用更新函数修改设备字段
	updater(device)
	// 更新最后出现时间
	device.LastSeenTime = time.Now()
	return true
}

