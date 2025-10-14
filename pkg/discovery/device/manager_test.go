package device

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/junbin-yang/nstackx-go/api"
)

// createTestDevice 创建测试用的设备信息
// 参数id：设备唯一标识（DeviceID）
// 返回值：构造好的测试设备信息
func createTestDevice(id string) *api.DeviceInfo {
	return &api.DeviceInfo{
		DeviceID:      id,
		DeviceName:    "Test Device " + id,  // 设备名称，包含ID便于区分
		DeviceType:    api.DeviceTypePhone,   // 设备类型设为手机（测试用）
		NetworkIP:     net.ParseIP("192.168.1." + id), // 构造测试IP（如192.168.1.1）
		NetworkName:   "eth0",               // 网络接口名称（测试用）
		DiscoveryType: api.DiscoveryTypeActive, // 发现方式设为主动发现
		BusinessType:  api.BusinessTypeSoftbus, // 业务类型设为软总线（测试用）
		LastSeenTime:  time.Now(),           // 最后出现时间设为当前时间
	}
}

// TestManager_UpdateDevice 测试设备的添加和更新功能
func TestManager_UpdateDevice(t *testing.T) {
	manager := NewManager(10, 60*time.Second) // 初始化管理器，最大10台设备，老化时间60秒

	// 测试添加新设备
	device1 := createTestDevice("1")
	isNew := manager.UpdateDevice(device1)
	assert.True(t, isNew, "添加新设备时应返回true")
	assert.Equal(t, 1, manager.GetDeviceCount(), "添加后设备数量应为1")

	// 测试更新已有设备
	device1Updated := createTestDevice("1")
	device1Updated.DeviceName = "Updated Device 1" // 修改设备名称
	isNew = manager.UpdateDevice(device1Updated)
	assert.False(t, isNew, "更新已有设备时应返回false")
	assert.Equal(t, 1, manager.GetDeviceCount(), "更新后设备数量应不变")

	// 验证更新结果
	retrieved := manager.GetDevice("1")
	assert.NotNil(t, retrieved, "应能查询到更新后的设备")
	assert.Equal(t, "Updated Device 1", retrieved.DeviceName, "设备名称应已更新")
}

// TestManager_MaxDevices 测试最大设备数量限制功能
func TestManager_MaxDevices(t *testing.T) {
	maxDevices := uint32(3)
	manager := NewManager(maxDevices, 60*time.Second) // 最大设备数设为3

	// 添加设备直到达到最大值
	for i := 1; i <= 3; i++ {
		device := createTestDevice(string(rune('0' + i))) // 设备ID为"1"、"2"、"3"
		manager.UpdateDevice(device)
	}
	assert.Equal(t, 3, manager.GetDeviceCount(), "达到最大数量后设备数应为3")

	// 再添加1台设备，此时应移除最旧的设备（ID为"1"）
	device4 := createTestDevice("4")
	manager.UpdateDevice(device4)
	assert.Equal(t, 3, manager.GetDeviceCount(), "超过最大数量后设备数应保持3")

	// 验证最旧设备已被移除，新设备已添加
	assert.Nil(t, manager.GetDevice("1"), "最旧的设备1应被移除")
	assert.NotNil(t, manager.GetDevice("4"), "新设备4应添加成功")
}

// TestManager_GetDevice 测试通过设备ID查询设备的功能
func TestManager_GetDevice(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 测试查询不存在的设备
	device := manager.GetDevice("nonexistent")
	assert.Nil(t, device, "查询不存在的设备应返回nil")

	// 添加设备后查询
	testDevice := createTestDevice("1")
	manager.UpdateDevice(testDevice)

	retrieved := manager.GetDevice("1")
	require.NotNil(t, retrieved, "应能查询到已添加的设备")
	assert.Equal(t, testDevice.DeviceID, retrieved.DeviceID, "设备ID应匹配")
	assert.Equal(t, testDevice.DeviceName, retrieved.DeviceName, "设备名称应匹配")
}

// TestManager_GetAllDevices 测试获取所有设备的功能
func TestManager_GetAllDevices(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 测试空管理器
	devices := manager.GetAllDevices()
	assert.Empty(t, devices, "空管理器应返回空列表")

	// 添加多个设备后查询
	for i := 1; i <= 5; i++ {
		device := createTestDevice(string(rune('0' + i))) // ID为"1"到"5"
		manager.UpdateDevice(device)
	}

	devices = manager.GetAllDevices()
	assert.Len(t, devices, 5, "应返回5台设备")
}

// TestManager_RemoveDevice 测试移除设备的功能
func TestManager_RemoveDevice(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 测试移除不存在的设备
	removed := manager.RemoveDevice("nonexistent")
	assert.False(t, removed, "移除不存在的设备应返回false")

	// 添加设备后移除
	device := createTestDevice("1")
	manager.UpdateDevice(device)
	assert.Equal(t, 1, manager.GetDeviceCount(), "添加后设备数应为1")

	removed = manager.RemoveDevice("1")
	assert.True(t, removed, "移除存在的设备应返回true")
	assert.Equal(t, 0, manager.GetDeviceCount(), "移除后设备数应为0")
	assert.Nil(t, manager.GetDevice("1"), "移除后不应再查询到该设备")
}

// TestManager_CleanupExpired 测试清理过期设备的功能
func TestManager_CleanupExpired(t *testing.T) {
	manager := NewManager(10, 500*time.Millisecond) // 老化时间设为500毫秒（便于测试）

	// 添加设备并设置不同的最后出现时间
	device1 := createTestDevice("1")
	device2 := createTestDevice("2")
	device3 := createTestDevice("3")
	manager.UpdateDevice(device1)
	manager.UpdateDevice(device2)
	manager.UpdateDevice(device3)

	// 等待600毫秒（超过老化时间500毫秒），让设备1、3自然过期
	// （设备2在等待期间主动更新，保持活跃）
	time.Sleep(600 * time.Millisecond)

	// 手动更新设备2，使其LastSeenTime刷新为当前时间（避免过期）
	manager.UpdateDevice(device2)

	// 清理过期设备
	expired := manager.CleanupExpired()
	assert.Len(t, expired, 2, "应清理2台过期设备")
	assert.Contains(t, expired, "1", "设备1应被清理")
	assert.Contains(t, expired, "3", "设备3应被清理")

	// 验证剩余设备
	assert.Equal(t, 1, manager.GetDeviceCount(), "清理后应剩余1台设备")
	assert.NotNil(t, manager.GetDevice("2"), "未过期的设备2应保留")
}

// TestManager_SetAgingTime 测试更新设备老化时间的功能
func TestManager_SetAgingTime(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 修改老化时间为500毫秒
	manager.SetAgingTime(500 * time.Millisecond)

	// 添加1台2秒前最后出现的设备（已过期）
	device := createTestDevice("1")
	manager.UpdateDevice(device)

	// 等待600毫秒（超过老化时间），让设备自然过期
	time.Sleep(600 * time.Millisecond)

	// 清理应立即移除该设备
	expired := manager.CleanupExpired()
	assert.Len(t, expired, 1, "应清理1台过期设备")
	assert.Equal(t, "1", expired[0], "设备1应被清理")
}

// TestManager_SetMaxDevices 测试更新最大设备数量的功能
func TestManager_SetMaxDevices(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 添加5台设备
	for i := 1; i <= 5; i++ {
		device := createTestDevice(string(rune('0' + i))) // ID为"1"到"5"
		manager.UpdateDevice(device)
	}
	assert.Equal(t, 5, manager.GetDeviceCount(), "添加后设备数应为5")

	// 将最大设备数改为3（当前设备数超过新限制）
	manager.SetMaxDevices(3)
	assert.Equal(t, 3, manager.GetDeviceCount(), "调整后设备数应为3")

	// 验证最旧的2台设备已被移除
	assert.Nil(t, manager.GetDevice("1"), "最旧的设备1应被移除")
	assert.Nil(t, manager.GetDevice("2"), "较旧的设备2应被移除")
	assert.NotNil(t, manager.GetDevice("3"), "设备3应保留")
	assert.NotNil(t, manager.GetDevice("4"), "设备4应保留")
	assert.NotNil(t, manager.GetDevice("5"), "设备5应保留")
}

// TestManager_Clear 测试清空所有设备的功能
func TestManager_Clear(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 添加3台设备
	for i := 1; i <= 3; i++ {
		device := createTestDevice(string(rune('0' + i)))
		manager.UpdateDevice(device)
	}
	assert.Equal(t, 3, manager.GetDeviceCount(), "添加后设备数应为3")

	// 清空所有设备
	manager.Clear()
	assert.Equal(t, 0, manager.GetDeviceCount(), "清空后设备数应为0")
	assert.Empty(t, manager.GetAllDevices(), "清空后设备列表应为空")
}

// TestManager_FilterDevices 测试按条件筛选设备的功能
func TestManager_FilterDevices(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 添加不同类型的设备
	device1 := createTestDevice("1")
	device1.DeviceType = api.DeviceTypePhone // 手机类型
	manager.UpdateDevice(device1)

	device2 := createTestDevice("2")
	device2.DeviceType = api.DeviceTypePC // 电脑类型
	manager.UpdateDevice(device2)

	device3 := createTestDevice("3")
	device3.DeviceType = api.DeviceTypePhone // 手机类型
	manager.UpdateDevice(device3)

	// 筛选手机类型的设备
	phones := manager.FilterDevices(func(d *api.DeviceInfo) bool {
		return d.DeviceType == api.DeviceTypePhone
	})
	assert.Len(t, phones, 2, "应筛选出2台手机设备")

	// 筛选电脑类型的设备
	pcs := manager.FilterDevices(func(d *api.DeviceInfo) bool {
		return d.DeviceType == api.DeviceTypePC
	})
	assert.Len(t, pcs, 1, "应筛选出1台电脑设备")
	assert.Equal(t, "2", pcs[0].DeviceID, "筛选出的电脑设备ID应为2")
}

// TestManager_UpdateDeviceField 测试更新设备特定字段的功能
func TestManager_UpdateDeviceField(t *testing.T) {
	manager := NewManager(10, 60*time.Second)

	// 更新不存在的设备
	updated := manager.UpdateDeviceField("nonexistent", func(d *api.DeviceInfo) {
		d.DeviceName = "Updated"
	})
	assert.False(t, updated, "更新不存在的设备应返回false")

	// 添加设备后更新特定字段
	device := createTestDevice("1")
	manager.UpdateDevice(device)

	updated = manager.UpdateDeviceField("1", func(d *api.DeviceInfo) {
		d.DeviceName = "Updated Name"       // 更新设备名称
		d.BusinessType = api.BusinessTypeNearby // 更新业务类型
	})
	assert.True(t, updated, "更新存在的设备应返回true")

	// 验证更新结果
	retrieved := manager.GetDevice("1")
	assert.Equal(t, "Updated Name", retrieved.DeviceName, "设备名称应已更新")
	assert.Equal(t, api.BusinessTypeNearby, retrieved.BusinessType, "业务类型应已更新")
}

// TestManager_ConcurrentAccess 测试多协程并发访问的安全性
func TestManager_ConcurrentAccess(t *testing.T) {
	manager := NewManager(100, 60*time.Second)
	done := make(chan bool)

	// 协程1：并发更新设备
	go func() {
		for i := 0; i < 50; i++ {
			device := createTestDevice(string(rune('A' + i%26))) // ID循环使用A-Z
			manager.UpdateDevice(device)
		}
		done <- true
	}()

	// 协程2：并发查询设备
	go func() {
		for i := 0; i < 50; i++ {
			manager.GetDevice(string(rune('A' + i%26))) // 查询循环的ID
			manager.GetAllDevices()                     // 查询所有设备
		}
		done <- true
	}()

	// 协程3：并发清理过期设备
	go func() {
		for i := 0; i < 10; i++ {
			manager.CleanupExpired()
			time.Sleep(10 * time.Millisecond) // 间隔短时间，模拟频繁清理
		}
		done <- true
	}()

	// 等待所有协程完成
	for i := 0; i < 3; i++ {
		<-done
	}

	// 验证并发访问未崩溃，且有设备留存
	assert.True(t, manager.GetDeviceCount() > 0, "并发访问后应至少有1台设备")
}

// BenchmarkManager_UpdateDevice 基准测试：测试UpdateDevice方法的性能
func BenchmarkManager_UpdateDevice(b *testing.B) {
	manager := NewManager(1000, 60*time.Second)
	device := createTestDevice("1")

	b.ResetTimer() // 重置计时器，排除初始化时间
	for i := 0; i < b.N; i++ {
		device.DeviceID = string(rune(i % 1000)) // 循环使用1000个ID
		manager.UpdateDevice(device)
	}
}

// BenchmarkManager_GetDevice 基准测试：测试GetDevice方法的性能
func BenchmarkManager_GetDevice(b *testing.B) {
	manager := NewManager(1000, 60*time.Second)
	
	// 预先添加100台设备
	for i := 0; i < 100; i++ {
		device := createTestDevice(string(rune(i)))
		manager.UpdateDevice(device)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manager.GetDevice(string(rune(i % 100))) // 循环查询100个已存在的ID
	}
}

// BenchmarkManager_FilterDevices 基准测试：测试FilterDevices方法的性能
func BenchmarkManager_FilterDevices(b *testing.B) {
	manager := NewManager(1000, 60*time.Second)
	
	// 预先添加100台设备，一半手机一半电脑
	for i := 0; i < 100; i++ {
		device := createTestDevice(string(rune(i)))
		if i%2 == 0 {
			device.DeviceType = api.DeviceTypePhone // 偶数ID为手机
		} else {
			device.DeviceType = api.DeviceTypePC // 奇数ID为电脑
		}
		manager.UpdateDevice(device)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 筛选手机类型的设备
		manager.FilterDevices(func(d *api.DeviceInfo) bool {
			return d.DeviceType == api.DeviceTypePhone
		})
	}
}

