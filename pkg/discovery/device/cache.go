// 提供带有LRU淘汰策略的高级设备缓存功能
// 在需要设备缓存 / 高频访问 / 智能淘汰的场景下，DeviceCache 功能更强大且适合。
// 如果只是需要简单的设备列表管理（无高频访问需求），Manager 的轻量特性可能更合适。
// 但大多数实际场景中，DeviceCache 的增强特性（尤其是 LRU 和自动清理）会带来更优的性能和可维护性。
package device

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// CacheEntry 表示缓存中的设备条目，包含设备信息及缓存元数据
type CacheEntry struct {
	Device      *api.DeviceInfo   // 设备信息
	LastAccess  time.Time         // 最后访问时间（用于LRU策略）
	AccessCount uint64            // 访问次数（用于统计最常访问设备）
	element     *list.Element     // LRU列表中的元素指针（用于快速移动位置）
}

// DeviceCache 实现了带TTL过期机制的LRU设备缓存，用于高效管理设备信息
type DeviceCache struct {
	mu sync.RWMutex // 读写锁，保证并发安全

	// 缓存存储
	devices  map[string]*CacheEntry // 设备映射表（键：设备ID，值：缓存条目）
	lruList  *list.List             // LRU列表（维护设备访问顺序，头部为最近访问，尾部为最久未访问）

	// 配置参数
	maxSize   int                   // 缓存最大容量（超过时触发LRU淘汰）
	ttl       time.Duration         // 设备存活时间（超过此时长未更新则过期）
	onEvict   func(device *api.DeviceInfo) // 设备被淘汰时的回调函数

	// 统计信息
	hits      uint64                // 缓存命中次数
	misses    uint64                // 缓存未命中次数
	evictions uint64                // 设备被淘汰的总次数

	// 持久化配置
	persistPath string              // 缓存持久化路径（从磁盘加载/保存）
	autoPersist bool                // 是否自动持久化缓存

	log *logger.Logger              // 日志组件
}

// CacheConfig 包含缓存的配置参数
type CacheConfig struct {
	MaxSize     int                  // 缓存最大容量
	TTL         time.Duration        // 设备存活时间（TTL）
	OnEvict     func(device *api.DeviceInfo) // 设备被淘汰时的回调函数
	PersistPath string               // 持久化文件路径
	AutoPersist bool                 // 是否自动持久化
}

// NewDeviceCache 创建一个新的设备缓存
func NewDeviceCache(config *CacheConfig) *DeviceCache {
	if config == nil {
		config = &CacheConfig{
			MaxSize: 100,         // 默认最大容量100
			TTL:     60 * time.Second, // 默认TTL 60秒
		}
	}

	if config.MaxSize <= 0 {
		config.MaxSize = 100 // 确保最大容量为正数
	}

	cache := &DeviceCache{
		devices:     make(map[string]*CacheEntry),
		lruList:     list.New(),
		maxSize:     config.MaxSize,
		ttl:         config.TTL,
		onEvict:     config.OnEvict,
		persistPath: config.PersistPath,
		autoPersist: config.AutoPersist,
		log:         logger.Default(),
	}

	// 如果启用自动持久化且路径存在，从磁盘加载缓存
	if config.PersistPath != "" && config.AutoPersist {
		if err := cache.loadFromDisk(); err != nil {
			cache.log.Warn("从磁盘加载缓存失败", zap.Error(err))
		}
	}

	return cache
}

// Get 从缓存中获取设备信息
// 参数deviceID：设备唯一标识
// 返回值：设备信息（副本，避免并发修改）和是否存在（未过期）的标志
func (dc *DeviceCache) Get(deviceID string) (*api.DeviceInfo, bool) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	entry, exists := dc.devices[deviceID]
	if !exists {
		dc.misses++ // 未命中计数+1
		return nil, false
	}

	// 检查是否过期（TTL生效且超过存活时间）
	if dc.ttl > 0 && time.Since(entry.Device.LastSeenTime) > dc.ttl {
		dc.removeEntry(deviceID) // 移除过期条目
		dc.misses++
		return nil, false
	}

	// 更新访问元数据（用于LRU和统计）
	entry.LastAccess = time.Now()
	entry.AccessCount++
	
	// 将设备移到LRU列表头部（标记为最近访问）
	dc.lruList.MoveToFront(entry.element)
	
	dc.hits++ // 命中计数+1
	
	// 返回设备副本，避免外部修改缓存内数据
	deviceCopy := *entry.Device
	return &deviceCopy, true
}

// Put 向缓存中添加或更新设备
// 参数device：要添加/更新的设备信息
// 返回值：错误（如设备无效）
func (dc *DeviceCache) Put(device *api.DeviceInfo) error {
	if device == nil || device.DeviceID == "" {
		return fmt.Errorf("无效的设备（为空或无ID）")
	}

	dc.mu.Lock()
	defer dc.mu.Unlock()

	now := time.Now()
	device.LastSeenTime = now // 更新设备最后出现时间

	// 若设备已存在，更新信息
	if entry, exists := dc.devices[device.DeviceID]; exists {
		entry.Device = device
		entry.LastAccess = now
		entry.AccessCount++
		
		// 移到LRU列表头部（标记为最近访问）
		dc.lruList.MoveToFront(entry.element)
		
		dc.log.Debug("缓存中更新设备", zap.String("deviceID", device.DeviceID))
	} else {
		// 若缓存已满，触发LRU淘汰（移除最久未访问设备）
		if len(dc.devices) >= dc.maxSize {
			dc.evictLRU()
		}
		
		// 创建新缓存条目
		entry := &CacheEntry{
			Device:      device,
			LastAccess:  now,
			AccessCount: 1, // 初始访问次数为1
		}
		
		// 添加到LRU列表头部（新设备为最近访问）
		entry.element = dc.lruList.PushFront(device.DeviceID)
		
		// 添加到缓存映射表
		dc.devices[device.DeviceID] = entry
		
		dc.log.Debug("缓存中添加设备",
			zap.String("deviceID", device.DeviceID),
			zap.Int("缓存大小", len(dc.devices)))
	}

	// 若启用自动持久化，异步保存到磁盘
	if dc.autoPersist && dc.persistPath != "" {
		go dc.persistToDisk()
	}

	return nil
}

// Remove 从缓存中移除指定设备
// 参数deviceID：设备唯一标识
// 返回值：是否成功移除（设备存在则为true）
func (dc *DeviceCache) Remove(deviceID string) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	return dc.removeEntry(deviceID)
}

// removeEntry 移除缓存条目（需在已加锁的情况下调用）
func (dc *DeviceCache) removeEntry(deviceID string) bool {
	entry, exists := dc.devices[deviceID]
	if !exists {
		return false
	}

	// 从LRU列表中移除
	dc.lruList.Remove(entry.element)
	
	// 触发淘汰回调（若配置）
	if dc.onEvict != nil {
		dc.onEvict(entry.Device)
	}
	
	// 从映射表中移除
	delete(dc.devices, deviceID)
	
	dc.log.Debug("从缓存中移除设备", zap.String("deviceID", deviceID))
	
	return true
}

// evictLRU 淘汰最久未使用的设备（LRU策略）
func (dc *DeviceCache) evictLRU() {
	if dc.lruList.Len() == 0 {
		return
	}

	// LRU列表尾部为最久未访问的设备
	element := dc.lruList.Back()
	if element == nil {
		return
	}

	deviceID := element.Value.(string)
	entry := dc.devices[deviceID]
	
	// 触发淘汰回调（若配置）
	if dc.onEvict != nil && entry != nil {
		dc.onEvict(entry.Device)
	}
	
	// 从缓存中移除
	dc.lruList.Remove(element)
	delete(dc.devices, deviceID)
	
	dc.evictions++ // 淘汰计数+1
	
	dc.log.Debug("LRU策略淘汰设备", zap.String("deviceID", deviceID))
}

// Evict 清理所有过期的设备（超过TTL未更新）
// 返回值：被清理的设备数量
func (dc *DeviceCache) Evict() int {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.ttl <= 0 {
		return 0 // TTL未启用，无需清理
	}

	now := time.Now()
	evicted := 0
	toRemove := make([]string, 0) // 存储需要移除的设备ID

	// 筛选过期设备
	for deviceID, entry := range dc.devices {
		if now.Sub(entry.Device.LastSeenTime) > dc.ttl {
			toRemove = append(toRemove, deviceID)
		}
	}

	// 移除所有过期设备
	for _, deviceID := range toRemove {
		if dc.removeEntry(deviceID) {
			evicted++
			dc.evictions++
		}
	}

	if evicted > 0 {
		dc.log.Info("清理过期设备", zap.Int("数量", evicted))
	}

	return evicted
}

// Clear 清空缓存中的所有设备
func (dc *DeviceCache) Clear() {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// 对所有设备触发淘汰回调（若配置）
	if dc.onEvict != nil {
		for _, entry := range dc.devices {
			dc.onEvict(entry.Device)
		}
	}

	// 重置缓存
	dc.devices = make(map[string]*CacheEntry)
	dc.lruList = list.New()
	
	dc.log.Info("设备缓存已清空")
}

// GetAll 返回缓存中所有未过期的设备
func (dc *DeviceCache) GetAll() []api.DeviceInfo {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	devices := make([]api.DeviceInfo, 0, len(dc.devices))
	now := time.Now()

	for _, entry := range dc.devices {
		// 跳过过期设备
		if dc.ttl > 0 && now.Sub(entry.Device.LastSeenTime) > dc.ttl {
			continue
		}
		
		devices = append(devices, *entry.Device)
	}

	return devices
}

// GetAllEntries 返回缓存中所有条目（包含元数据）
func (dc *DeviceCache) GetAllEntries() map[string]*CacheEntry {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	entries := make(map[string]*CacheEntry)
	for id, entry := range dc.devices {
		// 创建副本，避免外部修改缓存内元数据
		entryCopy := &CacheEntry{
			Device:      entry.Device,
			LastAccess:  entry.LastAccess,
			AccessCount: entry.AccessCount,
		}
		entries[id] = entryCopy
	}

	return entries
}

// Size 返回当前缓存中的设备数量
func (dc *DeviceCache) Size() int {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	return len(dc.devices)
}

// SetMaxSize 更新缓存的最大容量
// 参数maxSize：新的最大容量（需为正数）
func (dc *DeviceCache) SetMaxSize(maxSize int) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if maxSize <= 0 {
		return
	}

	dc.maxSize = maxSize

	// 若当前容量超过新限制，触发LRU淘汰直到容量符合要求
	for len(dc.devices) > dc.maxSize {
		dc.evictLRU()
	}
}

// SetTTL 更新设备的存活时间（TTL）
// 参数ttl：新的存活时间
func (dc *DeviceCache) SetTTL(ttl time.Duration) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	dc.ttl = ttl
}

// GetStatistics 返回缓存的统计信息
func (dc *DeviceCache) GetStatistics() CacheStatistics {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	total := dc.hits + dc.misses
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(dc.hits) / float64(total) * 100 // 命中率（百分比）
	}

	return CacheStatistics{
		Size:      len(dc.devices),   // 当前缓存大小
		MaxSize:   dc.maxSize,        // 最大容量
		Hits:      dc.hits,           // 命中次数
		Misses:    dc.misses,         // 未命中次数
		Evictions: dc.evictions,      // 淘汰次数
		HitRate:   hitRate,           // 命中率（%）
		TTL:       dc.ttl,            // 存活时间
	}
}

// CacheStatistics 包含缓存的统计信息
type CacheStatistics struct {
	Size      int           // 当前缓存大小
	MaxSize   int           // 最大容量
	Hits      uint64        // 命中次数
	Misses    uint64        // 未命中次数
	Evictions uint64        // 淘汰次数
	HitRate   float64       // 命中率（百分比）
	TTL       time.Duration // 存活时间
}

// Touch 更新设备的访问时间（标记为最近访问）
// 参数deviceID：设备唯一标识
// 返回值：是否更新成功（设备存在则为true）
func (dc *DeviceCache) Touch(deviceID string) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	entry, exists := dc.devices[deviceID]
	if !exists {
		return false
	}

	entry.LastAccess = time.Now()
	entry.AccessCount++
	
	// 移到LRU列表头部（标记为最近访问）
	dc.lruList.MoveToFront(entry.element)
	
	return true
}

// Filter 返回符合过滤条件的未过期设备
// 参数filter：过滤函数（返回true表示保留该设备）
func (dc *DeviceCache) Filter(filter func(*api.DeviceInfo) bool) []api.DeviceInfo {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	filtered := make([]api.DeviceInfo, 0)
	now := time.Now()

	for _, entry := range dc.devices {
		// 跳过过期设备
		if dc.ttl > 0 && now.Sub(entry.Device.LastSeenTime) > dc.ttl {
			continue
		}
		
		if filter(entry.Device) {
			filtered = append(filtered, *entry.Device)
		}
	}

	return filtered
}

// GetMostAccessed 返回访问次数最多的前N个设备
// 参数count：需要返回的设备数量
func (dc *DeviceCache) GetMostAccessed(count int) []api.DeviceInfo {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	// 将缓存条目转换为切片（便于排序）
	entries := make([]*CacheEntry, 0, len(dc.devices))
	for _, entry := range dc.devices {
		entries = append(entries, entry)
	}

	// 按访问次数降序排序（简单冒泡排序，适合小规模数据）
	for i := 0; i < len(entries)-1; i++ {
		for j := 0; j < len(entries)-i-1; j++ {
			if entries[j].AccessCount < entries[j+1].AccessCount {
				entries[j], entries[j+1] = entries[j+1], entries[j]
			}
		}
	}

	// 取前N个设备
	result := make([]api.DeviceInfo, 0, count)
	for i := 0; i < count && i < len(entries); i++ {
		result = append(result, *entries[i].Device)
	}

	return result
}

// 持久化相关方法（本示例中简化实现）

// loadFromDisk 从磁盘加载缓存（未实现具体逻辑）
func (dc *DeviceCache) loadFromDisk() error {
	// TODO: 实现缓存持久化逻辑
	// 用于启动时从磁盘加载缓存的设备数据
	return nil
}

// persistToDisk 将缓存保存到磁盘（未实现具体逻辑）
func (dc *DeviceCache) persistToDisk() error {
	// TODO: 实现缓存持久化逻辑
	// 用于定期将缓存数据保存到磁盘
	return nil
}

// StartCleanupWorker 启动后台工作线程，定期清理过期设备
// 参数interval：清理间隔时间
// 返回值：用于停止工作线程的通道
func (dc *DeviceCache) StartCleanupWorker(interval time.Duration) chan struct{} {
	stop := make(chan struct{})
	
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C: // 定时触发清理
				evicted := dc.Evict()
				if evicted > 0 {
					dc.log.Debug("清理工作线程淘汰设备", zap.Int("数量", evicted))
				}
			case <-stop: // 接收停止信号
				return
			}
		}
	}()
	
	return stop
}

