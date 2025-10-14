package device

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/junbin-yang/nstackx-go/api"
)

/*
// createTestDevice 生成测试用的设备信息
func createTestDevice(id string) *api.DeviceInfo {
	return &api.DeviceInfo{
		DeviceID:      id,
		DeviceName:    "TestDevice_" + id,
		DeviceType:    api.DeviceTypePhone,
		NetworkIP:     net.ParseIP("192.168.1." + id),
		LastSeenTime:  time.Now(),
		BusinessType:  api.BusinessTypeSoftbus,
		DiscoveryType: api.DiscoveryTypeActive,
	}
}
*/

// TestDeviceCache_Init 测试缓存初始化（默认配置和自定义配置）
func TestDeviceCache_Init(t *testing.T) {
	// 测试默认配置
	defaultCache := NewDeviceCache(nil)
	assert.Equal(t, 100, defaultCache.maxSize, "默认最大容量应为100")
	assert.Equal(t, 60*time.Second, defaultCache.ttl, "默认TTL应为60秒")
	assert.NotNil(t, defaultCache.log, "日志组件不应为nil")

	// 测试自定义配置
	customConfig := &CacheConfig{
		MaxSize: 200,
		TTL:     30 * time.Second,
	}
	customCache := NewDeviceCache(customConfig)
	assert.Equal(t, 200, customCache.maxSize, "自定义最大容量应为200")
	assert.Equal(t, 30*time.Second, customCache.ttl, "自定义TTL应为30秒")
}

// TestDeviceCache_PutAndGet 测试添加和查询设备
func TestDeviceCache_PutAndGet(t *testing.T) {
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 60 * time.Second})
	device := createTestDevice("1")

	// 添加设备
	err := cache.Put(device)
	assert.NoError(t, err, "添加设备不应出错")
	assert.Equal(t, 1, cache.Size(), "缓存大小应为1")

	// 查询存在的设备
	retrieved, exists := cache.Get("1")
	assert.True(t, exists, "应查询到设备1")
	assert.Equal(t, device.DeviceID, retrieved.DeviceID, "设备ID应匹配")
	assert.Equal(t, device.DeviceName, retrieved.DeviceName, "设备名称应匹配")

	// 查询不存在的设备
	_, exists = cache.Get("999")
	assert.False(t, exists, "不应查询到不存在的设备999")

	// 验证统计信息（1次命中，1次未命中）
	stats := cache.GetStatistics()
	assert.Equal(t, uint64(1), stats.Hits, "命中次数应为1")
	assert.Equal(t, uint64(1), stats.Misses, "未命中次数应为1")
}

// TestDeviceCache_LRUEviction 测试LRU淘汰策略（缓存满时淘汰最久未访问设备）
func TestDeviceCache_LRUEviction(t *testing.T) {
	// 最大容量设为2，便于测试LRU淘汰
	cache := NewDeviceCache(&CacheConfig{MaxSize: 2, TTL: 0}) // TTL=0表示不过期

	// 添加2台设备（达到最大容量）
	d1 := createTestDevice("1")
	d2 := createTestDevice("2")
	cache.Put(d1)
	cache.Put(d2)
	assert.Equal(t, 2, cache.Size(), "初始缓存大小应为2")

	// 访问设备1（标记为最近访问，设备2变为最久未访问）
	_, _ = cache.Get("1")

	// 添加第3台设备，触发LRU淘汰（应淘汰设备2）
	d3 := createTestDevice("3")
	cache.Put(d3)
	assert.Equal(t, 2, cache.Size(), "淘汰后缓存大小仍为2")

	// 验证设备2被淘汰，设备1和3存在
	_, exists := cache.Get("2")
	assert.False(t, exists, "设备2应被LRU淘汰")
	_, exists = cache.Get("1")
	assert.True(t, exists, "设备1应保留")
	_, exists = cache.Get("3")
	assert.True(t, exists, "设备3应保留")

	// 验证淘汰统计
	assert.Equal(t, uint64(1), cache.GetStatistics().Evictions, "淘汰次数应为1")
}

// TestDeviceCache_EvictExpired 测试过期设备清理（TTL机制）
func TestDeviceCache_EvictExpired(t *testing.T) {
	// TTL设为500ms（便于测试过期）
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 500 * time.Millisecond})

	// 添加3台设备
	d1 := createTestDevice("1")
	d2 := createTestDevice("2")
	d3 := createTestDevice("3")
	cache.Put(d1)
	cache.Put(d2)
	cache.Put(d3)
	assert.Equal(t, 3, cache.Size(), "初始缓存大小应为3")

	// 等待600ms（超过TTL），让所有设备过期
	time.Sleep(600 * time.Millisecond)

	// 清理过期设备
	evicted := cache.Evict()
	assert.Equal(t, 3, evicted, "应清理3台过期设备")
	assert.Equal(t, 0, cache.Size(), "清理后缓存应为空")

	// 验证查询已过期设备（应未命中）
	_, exists := cache.Get("1")
	assert.False(t, exists, "过期设备1不应被查询到")
}

// TestDeviceCache_Remove 测试手动移除设备
func TestDeviceCache_Remove(t *testing.T) {
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 60 * time.Second})
	device := createTestDevice("1")
	cache.Put(device)

	// 移除存在的设备
	removed := cache.Remove("1")
	assert.True(t, removed, "移除设备1应成功")
	assert.Equal(t, 0, cache.Size(), "缓存大小应为0")

	// 移除不存在的设备
	removed = cache.Remove("999")
	assert.False(t, removed, "移除不存在的设备应失败")
}

// TestDeviceCache_Filter 测试设备筛选功能
func TestDeviceCache_Filter(t *testing.T) {
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 60 * time.Second})

	// 添加不同类型的设备
	d1 := createTestDevice("1")
	d1.DeviceType = api.DeviceTypePhone
	d2 := createTestDevice("2")
	d2.DeviceType = api.DeviceTypePC
	d3 := createTestDevice("3")
	d3.DeviceType = api.DeviceTypePhone
	cache.Put(d1)
	cache.Put(d2)
	cache.Put(d3)

	// 筛选手机类型的设备（DeviceTypePhone）
	phones := cache.Filter(func(d *api.DeviceInfo) bool {
		return d.DeviceType == api.DeviceTypePhone
	})
	assert.Len(t, phones, 2, "应筛选出2台手机设备")

	// 检查结果是否包含设备1和3
	hasID1 := false
	hasID3 := false
	for _, d := range phones {
		if d.DeviceID == "1" {
			hasID1 = true
		}
		if d.DeviceID == "3" {
			hasID3 = true
		}
	}
	assert.True(t, hasID1, "筛选结果应包含设备1")
	assert.True(t, hasID3, "筛选结果应包含设备3")

	// 筛选电脑类型的设备（DeviceTypePC）
	pcs := cache.Filter(func(d *api.DeviceInfo) bool {
		return d.DeviceType == api.DeviceTypePC
	})
	assert.Len(t, pcs, 1, "应筛选出1台电脑设备")
	assert.Equal(t, "2", pcs[0].DeviceID, "筛选结果应包含设备2")
}

// TestDeviceCache_GetMostAccessed 测试获取最常访问设备
func TestDeviceCache_GetMostAccessed(t *testing.T) {
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 60 * time.Second})

	// 添加3台设备
	d1 := createTestDevice("1")
	d2 := createTestDevice("2")
	d3 := createTestDevice("3")
	cache.Put(d1)
	cache.Put(d2)
	cache.Put(d3)

	// 模拟访问：设备1访问3次，设备2访问2次，设备3访问1次
	for i := 0; i < 3; i++ {
		_, _ = cache.Get("1")
	}
	for i := 0; i < 2; i++ {
		_, _ = cache.Get("2")
	}
	_, _ = cache.Get("3")

	// 获取访问次数前2的设备
	top2 := cache.GetMostAccessed(2)
	assert.Len(t, top2, 2, "应返回2台设备")
	assert.Equal(t, "1", top2[0].DeviceID, "第1名应为设备1（访问3次）")
	assert.Equal(t, "2", top2[1].DeviceID, "第2名应为设备2（访问2次）")
}

// TestDeviceCache_CleanupWorker 测试后台清理工作线程
func TestDeviceCache_CleanupWorker(t *testing.T) {
	// TTL设为500ms，清理间隔300ms
	cache := NewDeviceCache(&CacheConfig{MaxSize: 10, TTL: 500 * time.Millisecond})
	stopChan := cache.StartCleanupWorker(300 * time.Millisecond)
	defer close(stopChan) // 测试结束后停止工作线程

	// 添加设备
	d1 := createTestDevice("1")
	cache.Put(d1)
	assert.Equal(t, 1, cache.Size(), "初始缓存大小应为1")

	// 等待700ms（超过TTL+清理间隔），确保工作线程已清理过期设备
	time.Sleep(700 * time.Millisecond)

	assert.Equal(t, 0, cache.Size(), "后台线程应清理过期设备")
}

// TestDeviceCache_Statistics 测试统计信息准确性
func TestDeviceCache_Statistics(t *testing.T) {
	cache := NewDeviceCache(&CacheConfig{MaxSize: 2, TTL: 60 * time.Second})

	// 添加2台设备
	d1 := createTestDevice("1")
	d2 := createTestDevice("2")
	cache.Put(d1)
	cache.Put(d2)

	// 访问设备1（第一次命中）
	_, _ = cache.Get("1") // hits=1

	// 添加第3台设备，触发LRU淘汰（淘汰设备2）
	d3 := createTestDevice("3")
	cache.Put(d3)

	// 再次访问设备1（第二次命中）
	_, _ = cache.Get("1") // hits=2
	// 访问设备3（第三次命中）
	_, _ = cache.Get("3") // hits=3
	// 访问不存在的设备（未命中）
	_, _ = cache.Get("999") // misses=1

	stats := cache.GetStatistics()
	assert.Equal(t, 2, stats.Size, "当前缓存大小应为2")
	assert.Equal(t, 2, stats.MaxSize, "最大容量应为2")
	assert.Equal(t, uint64(3), stats.Hits, "命中次数应为3")
	assert.Equal(t, uint64(1), stats.Misses, "未命中次数应为1")
	assert.Equal(t, uint64(1), stats.Evictions, "淘汰次数应为1")
	assert.InDelta(t, 75.0, stats.HitRate, 0.01, "命中率应约为75%")
}

