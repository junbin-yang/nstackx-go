// 提供定时器管理功能，包括周期性定时器、一次性定时器及相关工具函数 
package timer

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Timer struct {
	id       string          // 定时器唯一标识
	interval time.Duration   // 触发间隔（周期性定时器）或延迟时间（一次性定时器）
	callback func()          // 定时器触发时执行的回调函数
	ticker   *time.Ticker    // 周期性定时器的底层时钟（一次性定时器为nil）
	stopChan chan struct{}   // 用于停止定时器的信号通道
	once     sync.Once       // 确保Stop操作只执行一次（避免通道重复关闭）
}

type Manager struct {
	mu     sync.RWMutex      // 读写锁，保护timers map的并发访问
	timers map[string]*Timer // 存储所有定时器，key为定时器ID
	ctx    context.Context   // 用于通知所有定时器停止的上下文
	cancel context.CancelFunc // 用于触发全局停止的函数
}

// 创建一个新的定时器管理器
func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		timers: make(map[string]*Timer),
		ctx:    ctx,
		cancel: cancel,
	}
}

// 创建并启动一个周期性定时器
// 参数:
//   id: 定时器唯一标识
//   interval: 触发间隔
//   callback: 每次触发时执行的回调函数
// 返回: 若ID已存在则返回错误，否则返回nil
func (m *Manager) CreateTimer(id string, interval time.Duration, callback func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.timers[id]; exists {
		return fmt.Errorf("timer %s already exists", id)
	}

	timer := &Timer{
		id:       id,
		interval: interval,
		callback: callback,
		ticker:   time.NewTicker(interval), // 创建周期性时钟
		stopChan: make(chan struct{}),
	}

	m.timers[id] = timer

	// 处理周期性触发逻辑
	go m.runTimer(timer)

	return nil
}

// CreateOnceTimer 创建一个一次性定时器（延迟后触发一次）
// 参数:
//   id: 定时器唯一标识
//   delay: 延迟时间（多久后触发）
//   callback: 触发时执行的回调函数
// 返回: 若ID已存在则返回错误，否则返回nil
func (m *Manager) CreateOnceTimer(id string, delay time.Duration, callback func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.timers[id]; exists {
		return fmt.Errorf("timer %s already exists", id)
	}

	timer := &Timer{
		id:       id,
		interval: delay,
		callback: callback,
		stopChan: make(chan struct{}), // 一次性定时器无需ticker
	}

	m.timers[id] = timer

	// 启动一次性定时器协程
	go func() {
		select {
		case <-time.After(delay): // 延迟后执行回调
			callback()
			m.RemoveTimer(id) // 执行后自动移除
		case <-timer.stopChan: // 被主动停止
			return
		case <-m.ctx.Done(): // 管理器被停止
			return
		}
	}()

	return nil
}

// RemoveTimer 停止并移除指定ID的定时器
// 参数:
//   id: 要移除的定时器ID
// 返回: 若ID不存在则返回错误，否则返回nil
func (m *Manager) RemoveTimer(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	timer, exists := m.timers[id]
	if !exists {
		return fmt.Errorf("timer %s not found", id)
	}

	// 确保停止操作只执行一次（避免重复关闭通道）
	timer.once.Do(func() {
		if timer.ticker != nil {
			timer.ticker.Stop() // 停止周期性时钟
		}
		close(timer.stopChan) // 发送停止信号
	})

	delete(m.timers, id) // 从管理器中移除
	return nil
}

// StopAll 停止并移除所有定时器，同时终止管理器
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 逐个停止所有定时器
	for id, timer := range m.timers {
		timer.once.Do(func() {
			if timer.ticker != nil {
				timer.ticker.Stop()
			}
			close(timer.stopChan)
		})
		delete(m.timers, id)
	}

	m.cancel() // 触发全局上下文取消，通知所有相关协程退出
}

// ResetTimer 重置指定定时器的触发间隔（仅对周期性定时器有效）
// 参数:
//   id: 定时器ID
//   newInterval: 新的触发间隔
// 返回: 若ID不存在则返回错误，否则返回nil
func (m *Manager) ResetTimer(id string, newInterval time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	timer, exists := m.timers[id]
	if !exists {
		return fmt.Errorf("timer %s not found", id)
	}

	// 停止旧的周期性时钟
	if timer.ticker != nil {
		timer.ticker.Stop()
	}

	// 创建新的周期性时钟并更新间隔
	timer.interval = newInterval
	timer.ticker = time.NewTicker(newInterval)

	return nil
}

// GetTimer 获取指定ID的定时器信息
// 参数:
//   id: 定时器ID
// 返回: 定时器实例和是否存在的标志
func (m *Manager) GetTimer(id string) (*Timer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	timer, exists := m.timers[id]
	return timer, exists
}

// GetTimerCount 获取当前活跃的定时器数量
func (m *Manager) GetTimerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.timers)
}

// runTimer 周期性定时器的运行逻辑（内部协程函数）
func (m *Manager) runTimer(timer *Timer) {
	if timer.ticker == nil {
		return
	}

	// 循环等待触发信号或停止信号
	for {
		select {
		case <-timer.ticker.C: // 周期性触发，执行回调
			timer.callback()
		case <-timer.stopChan: // 被主动停止
			return
		case <-m.ctx.Done(): // 管理器被停止
			return
		}
	}
}

// ScheduleFunc 解析时间规格并创建周期性定时器（对CreateTimer的封装）
// 参数:
//   id: 定时器ID
//   spec: 时间规格（支持time.Duration格式，如"5s"、"1m"）
//   callback: 回调函数
// 返回: 解析失败或创建失败时返回错误
func (m *Manager) ScheduleFunc(id string, spec string, callback func()) error {
	// 解析时间规格为Duration
	duration, err := time.ParseDuration(spec)
	if err != nil {
		return fmt.Errorf("invalid duration spec: %w", err)
	}

	return m.CreateTimer(id, duration, callback)
}

// Debounce 创建一个防抖函数：多次调用时，仅在最后一次调用后等待wait时间再执行回调
// 应用场景：如搜索输入框，避免输入过程中频繁触发搜索
// 参数:
//   wait: 等待时间
//   callback: 最终执行的回调函数
// 返回: 包装后的防抖函数
func Debounce(wait time.Duration, callback func()) func() {
	var timer *time.Timer
	var mu sync.Mutex // 保护timer的并发访问

	return func() {
		mu.Lock()
		defer mu.Unlock()

		// 若已有定时器，先停止（重置等待时间）
		if timer != nil {
			timer.Stop()
		}

		// 创建新定时器，等待wait后执行回调
		timer = time.AfterFunc(wait, callback)
	}
}

// Throttle 创建一个节流函数：指定时间内最多执行一次回调
// 应用场景：如按钮点击，避免短时间内多次触发
// 参数:
//   duration: 节流时间窗口
//   callback: 被节流的回调函数
// 返回: 包装后的节流函数
func Throttle(duration time.Duration, callback func()) func() {
	var lastCall time.Time // 上次执行时间
	var mu sync.Mutex      // 保护lastCall的并发访问

	return func() {
		mu.Lock()
		defer mu.Unlock()

		now := time.Now()
		// 若距离上次执行已超过duration，则执行回调并更新时间
		if now.Sub(lastCall) >= duration {
			lastCall = now
			callback()
		}
	}
}

// Retry 带重试逻辑的函数执行：失败后重试指定次数，每次间隔固定时间
// 参数:
//   attempts: 最大尝试次数（含首次）
//   delay: 每次重试的间隔时间
//   fn: 待执行的函数（返回error表示失败）
// 返回: 若成功返回nil，否则返回最后一次错误
func Retry(attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil // 成功则直接返回
		}
		// 不是最后一次尝试则等待后重试
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return fmt.Errorf("after %d attempts, last error: %w", attempts, err)
}

// ExponentialBackoff 带指数退避的重试逻辑：每次重试间隔翻倍
// 应用场景：网络请求重试，减轻服务器压力
// 参数:
//   attempts: 最大尝试次数（含首次）
//   initialDelay: 初始延迟时间（后续每次翻倍）
//   fn: 待执行的函数（返回error表示失败）
// 返回: 若成功返回nil，否则返回最后一次错误
func ExponentialBackoff(attempts int, initialDelay time.Duration, fn func() error) error {
	var err error
	delay := initialDelay // 初始延迟

	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil // 成功则直接返回
		}
		// 不是最后一次尝试则等待后重试，延迟翻倍
		if i < attempts-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("after %d attempts with exponential backoff, last error: %w", attempts, err)
}

