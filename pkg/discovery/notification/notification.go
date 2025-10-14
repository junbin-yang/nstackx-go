// 设备发现的通知系统
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// NotificationManager 管理通知和订阅关系
type NotificationManager struct {
	mu sync.RWMutex // 读写锁，用于保护共享数据

	// 订阅相关
	subscriptions map[string][]*Subscription // 按主题划分的订阅列表（key: 主题）
	subscribers   map[string]*Subscriber     // 订阅者映射（key: 订阅者ID）

	// 通知队列
	queue        chan *Notification       // 待处理通知队列
	pendingQueue map[string][]*Notification // 待重试通知队列（key: 通知ID）

	// 配置参数
	maxQueueSize  int           // 队列最大容量
	maxRetries    int           // 最大重试次数
	retryInterval time.Duration // 重试间隔

	// 控制相关
	ctx    context.Context    // 上下文，用于控制协程退出
	cancel context.CancelFunc // 取消函数，用于停止管理器
	wg     sync.WaitGroup     // 等待组，用于等待工作协程退出

	// 统计信息
	stats NotificationStats // 通知统计数据

	log *logger.Logger // 日志工具
}

// Notification 表示一条通知消息
type Notification struct {
	ID           string        // 通知唯一标识
	Topic        string        // 通知主题（用于匹配订阅）
	Type         NotificationType // 通知类型
	Priority     Priority      // 优先级
	Payload      interface{}   // 通知内容
	Timestamp    time.Time     // 创建时间
	Expiry       time.Time     // 过期时间（过期后不再处理）
	Retries      int           // 已重试次数
	MaxRetries   int           // 最大重试次数
	NextRetry    time.Time     // 下次重试时间
	Destinations []string      // 特定接收者ID列表（为空则发给所有订阅者）
}

// NotificationType 表示通知的类型
type NotificationType string

const (
	NotificationTypeDevice    NotificationType = "device"    // 设备相关通知
	NotificationTypeDiscovery NotificationType = "discovery" // 发现相关通知
	NotificationTypeStatus    NotificationType = "status"    // 状态相关通知
	NotificationTypeError     NotificationType = "error"     // 错误相关通知
	NotificationTypeCustom    NotificationType = "custom"    // 自定义通知
)

// Priority 表示通知的优先级
type Priority int

const (
	PriorityLow      Priority = iota // 低优先级
	PriorityNormal                   // 正常优先级
	PriorityHigh                     // 高优先级
	PriorityCritical                 // 紧急优先级
)

// Subscription 表示一个主题订阅关系
type Subscription struct {
	ID         string        // 订阅唯一标识
	Topic      string        // 订阅的主题
	Subscriber *Subscriber   // 订阅者
	Filter     FilterFunc    // 过滤函数（用于筛选通知）
	Created    time.Time     // 订阅创建时间
	LastNotify time.Time     // 最后一次接收通知的时间
}

// Subscriber 表示一个通知订阅者
type Subscriber struct {
	ID       string         // 订阅者唯一标识
	Name     string         // 订阅者名称
	Callback NotifyCallback // 接收通知的回调函数
	Topics   []string       // 订阅的主题列表
	Active   bool           // 是否活跃
	Created  time.Time      // 创建时间
}

// NotifyCallback 订阅者接收通知时的回调函数类型
type NotifyCallback func(notification *Notification) error

// FilterFunc 用于过滤通知的函数类型（返回true则保留通知）
type FilterFunc func(notification *Notification) bool

// NotificationStats 包含通知相关的统计信息
type NotificationStats struct {
	TotalSent       uint64 // 总发送成功数
	TotalFailed     uint64 // 总发送失败数
	TotalRetried    uint64 // 总重试次数
	TotalDropped    uint64 // 总丢弃数（队列满、过期等）
	QueueSize       int    // 当前队列大小
	SubscriberCount int    // 订阅者数量
	TopicCount      int    // 主题数量
}

// Config 包含通知管理器的配置参数
type Config struct {
	MaxQueueSize  int           // 队列最大容量
	MaxRetries    int           // 默认最大重试次数
	RetryInterval time.Duration // 默认重试间隔
}

// NewNotificationManager 创建一个新的通知管理器
func NewNotificationManager(config *Config) *NotificationManager {
	if config == nil {
		config = &Config{
			MaxQueueSize:  1000,
			MaxRetries:    3,
			RetryInterval: 1 * time.Second,
		}
	}

	if config.MaxQueueSize <= 0 {
		config.MaxQueueSize = 1000
	}
	if config.MaxRetries < 0 { // 允许 0 次重试（不重试）
		config.MaxRetries = 0
	}
	if config.RetryInterval <= 0 { // 强制重试间隔为正数
		config.RetryInterval = 1 * time.Second // 使用默认值
	}

	ctx, cancel := context.WithCancel(context.Background())

	nm := &NotificationManager{
		subscriptions: make(map[string][]*Subscription),
		subscribers:   make(map[string]*Subscriber),
		queue:         make(chan *Notification, config.MaxQueueSize),
		pendingQueue:  make(map[string][]*Notification),
		maxQueueSize:  config.MaxQueueSize,
		maxRetries:    config.MaxRetries,
		retryInterval: config.RetryInterval,
		ctx:           ctx,
		cancel:        cancel,
		log:           logger.Default(),
	}

	// 启动工作协程
	nm.wg.Add(2)
	go nm.notificationWorker()  // 处理通知分发的工作协程
	go nm.retryWorker()         // 处理重试的工作协程

	return nm
}

// Subscribe 订阅一个主题（无过滤）
func (nm *NotificationManager) Subscribe(topic string, callback NotifyCallback) (string, error) {
	return nm.SubscribeWithFilter(topic, callback, nil)
}

// SubscribeWithFilter 订阅一个主题（带过滤函数）
func (nm *NotificationManager) SubscribeWithFilter(topic string, callback NotifyCallback, filter FilterFunc) (string, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if topic == "" || callback == nil {
		return "", fmt.Errorf("无效的订阅参数")
	}

	// 创建订阅者
	subscriberID := nm.generateID()
	subscriber := &Subscriber{
		ID:       subscriberID,
		Callback: callback,
		Topics:   []string{topic},
		Active:   true,
		Created:  time.Now(),
	}

	// 创建订阅关系
	subscription := &Subscription{
		ID:         nm.generateID(),
		Topic:      topic,
		Subscriber: subscriber,
		Filter:     filter,
		Created:    time.Now(),
	}

	// 添加到映射中
	nm.subscribers[subscriberID] = subscriber
	if nm.subscriptions[topic] == nil {
		nm.subscriptions[topic] = make([]*Subscription, 0)
	}
	nm.subscriptions[topic] = append(nm.subscriptions[topic], subscription)

	nm.log.Info("创建订阅",
		zap.String("id", subscription.ID),
		zap.String("topic", topic))

	return subscription.ID, nil
}

// Unsubscribe 取消订阅（通过订阅ID）
func (nm *NotificationManager) Unsubscribe(subscriptionID string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// 查找并移除订阅
	for topic, subs := range nm.subscriptions {
		for i, sub := range subs {
			if sub.ID == subscriptionID {
				// 从切片中移除
				nm.subscriptions[topic] = append(subs[:i], subs[i+1:]...)
				
				// 清理空主题
				if len(nm.subscriptions[topic]) == 0 {
					delete(nm.subscriptions, topic)
				}

				nm.log.Info("移除订阅",
					zap.String("id", subscriptionID),
					zap.String("topic", topic))
				
				return nil
			}
		}
	}

	return fmt.Errorf("订阅不存在: %s", subscriptionID)
}

// Publish 发布一条通知到指定主题
func (nm *NotificationManager) Publish(notification *Notification) error {
	if notification == nil {
		return fmt.Errorf("通知不能为nil")
	}

	// 设置默认值
	if notification.ID == "" {
		notification.ID = nm.generateID()
	}
	if notification.Timestamp.IsZero() {
		notification.Timestamp = time.Now()
	}
	if notification.MaxRetries == 0 {
		notification.MaxRetries = nm.maxRetries
	}

	// 将通知加入队列
	select {
	case nm.queue <- notification:
		nm.log.Debug("通知已加入队列",
			zap.String("id", notification.ID),
			zap.String("topic", notification.Topic))
		return nil
	default:
		nm.stats.TotalDropped++
		return fmt.Errorf("通知队列已满")
	}
}

// PublishDevice 发布设备相关通知
func (nm *NotificationManager) PublishDevice(device *api.DeviceInfo, eventType string) error {
	payload := map[string]interface{}{
		"event":  eventType,
		"device": device,
	}

	notification := &Notification{
		Topic:     "device." + eventType,
		Type:      NotificationTypeDevice,
		Priority:  PriorityNormal,
		Payload:   payload,
		Timestamp: time.Now(),
	}

	return nm.Publish(notification)
}

// PublishDiscovery 发布发现相关通知
func (nm *NotificationManager) PublishDiscovery(event string, data interface{}) error {
	notification := &Notification{
		Topic:     "discovery." + event,
		Type:      NotificationTypeDiscovery,
		Priority:  PriorityNormal,
		Payload:   data,
		Timestamp: time.Now(),
	}

	return nm.Publish(notification)
}

// PublishError 发布错误相关通知
func (nm *NotificationManager) PublishError(err error, context string) error {
	payload := map[string]interface{}{
		"error":   err.Error(),
		"context": context,
		"time":    time.Now(),
	}

	notification := &Notification{
		Topic:     "error",
		Type:      NotificationTypeError,
		Priority:  PriorityHigh,
		Payload:   payload,
		Timestamp: time.Now(),
	}

	return nm.Publish(notification)
}

// Broadcast 发送广播通知（所有订阅者都会收到）
func (nm *NotificationManager) Broadcast(notification *Notification) error {
	notification.Topic = "*" // 特殊主题表示广播
	return nm.Publish(notification)
}

// Stop 停止通知管理器
func (nm *NotificationManager) Stop() {
	nm.log.Info("停止通知管理器")
	
	nm.cancel()       // 触发上下文取消
	close(nm.queue)   // 关闭通知队列
	nm.wg.Wait()      // 等待工作协程退出
	
	nm.log.Info("通知管理器已停止",
		zap.Uint64("总发送数", nm.stats.TotalSent),
		zap.Uint64("总失败数", nm.stats.TotalFailed))
}

// GetStatistics 返回当前通知统计信息
func (nm *NotificationManager) GetStatistics() NotificationStats {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	nm.stats.QueueSize = len(nm.queue)
	nm.stats.SubscriberCount = len(nm.subscribers)
	nm.stats.TopicCount = len(nm.subscriptions)
	
	return nm.stats
}

// 工作协程相关方法

// notificationWorker 处理通知队列的工作协程
func (nm *NotificationManager) notificationWorker() {
	defer nm.wg.Done()

	for {
		select {
		case <-nm.ctx.Done(): // 上下文取消时退出
			return
		case notification, ok := <-nm.queue: // 从队列获取通知
			if !ok { // 队列关闭时退出
				return
			}
			nm.processNotification(notification) // 处理通知
		}
	}
}

// retryWorker 处理通知重试的工作协程
func (nm *NotificationManager) retryWorker() {
	defer nm.wg.Done()

	ticker := time.NewTicker(nm.retryInterval) // 定时触发重试检查
	defer ticker.Stop()

	for {
		select {
		case <-nm.ctx.Done(): // 上下文取消时退出
			return
		case <-ticker.C: // 定时检查并处理重试
			nm.processRetries()
		}
	}
}

// processNotification 处理单条通知（分发给订阅者）
func (nm *NotificationManager) processNotification(notification *Notification) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	// 检查通知是否过期
	if !notification.Expiry.IsZero() && time.Now().After(notification.Expiry) {
		nm.log.Debug("通知已过期",
			zap.String("id", notification.ID))
		nm.stats.TotalDropped++
		return
	}

	// 获取主题对应的订阅者
	var subscribers []*Subscription
	
	if notification.Topic == "*" {
		// 广播：收集所有主题的订阅者
		for _, subs := range nm.subscriptions {
			subscribers = append(subscribers, subs...)
		}
	} else {
		subscribers = nm.subscriptions[notification.Topic]
		
		// 同时包含订阅了通配符"*"的订阅者
		if wildcards, exists := nm.subscriptions["*"]; exists {
			subscribers = append(subscribers, wildcards...)
		}
	}

	if len(subscribers) == 0 {
		nm.log.Debug("主题无订阅者",
			zap.String("topic", notification.Topic))
		return
	}

	// 向每个订阅者发送通知
	var failed []string // 记录发送失败的订阅者ID
	for _, sub := range subscribers {
		// 应用过滤函数（过滤不匹配的通知）
		if sub.Filter != nil && !sub.Filter(notification) {
			continue
		}

		// 跳过非活跃订阅者
		if !sub.Subscriber.Active {
			continue
		}

		// 检查是否指定了接收者（仅发给指定接收者）
		if len(notification.Destinations) > 0 {
			found := false
			for _, dest := range notification.Destinations {
				if dest == sub.Subscriber.ID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// 调用订阅者的回调函数
		if err := sub.Subscriber.Callback(notification); err != nil {
			nm.log.Error("通知发送失败",
				zap.String("id", notification.ID),
				zap.String("subscriber", sub.Subscriber.ID),
				zap.Error(err))
			failed = append(failed, sub.Subscriber.ID)
			nm.stats.TotalFailed++
		} else {
			sub.LastNotify = time.Now()
			nm.stats.TotalSent++
		}
	}

	// 处理发送失败的通知（未超过最大重试次数则安排重试）
	if len(failed) > 0 && notification.Retries < notification.MaxRetries {
		nm.scheduleRetry(notification, failed)
	}
}

// processRetries 处理待重试的通知
func (nm *NotificationManager) processRetries() {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	now := time.Now()
	// 遍历所有待重试通知
	for id, notifications := range nm.pendingQueue {
		var remaining []*Notification // 未到重试时间的通知
		
		for _, n := range notifications {
			if now.After(n.NextRetry) {
				// 已到重试时间，重新加入队列处理
				n.Retries++
				nm.stats.TotalRetried++
				
				select {
				case nm.queue <- n:
					nm.log.Debug("通知已安排重试",
						zap.String("id", n.ID),
						zap.Int("尝试次数", n.Retries))
				default:
					nm.stats.TotalDropped++ // 队列满，丢弃
				}
			} else {
				remaining = append(remaining, n) // 保留未到时间的通知
			}
		}

		// 更新待重试队列（保留未处理的）
		if len(remaining) > 0 {
			nm.pendingQueue[id] = remaining
		} else {
			delete(nm.pendingQueue, id) // 无剩余则删除
		}
	}
}

// scheduleRetry 安排通知重试（计算下次重试时间并加入待重试队列）
func (nm *NotificationManager) scheduleRetry(notification *Notification, failed []string) {
	// 计算退避时间（指数退避：间隔 * 2^重试次数）
	backoff := nm.retryInterval * time.Duration(1<<notification.Retries)
	notification.NextRetry = time.Now().Add(backoff)
	notification.Destinations = failed // 仅重试失败的接收者

	// 加入待重试队列
	if nm.pendingQueue[notification.ID] == nil {
		nm.pendingQueue[notification.ID] = make([]*Notification, 0)
	}
	nm.pendingQueue[notification.ID] = append(nm.pendingQueue[notification.ID], notification)
}

// generateID 生成唯一ID（基于当前时间和订阅者数量）
func (nm *NotificationManager) generateID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(nm.subscribers))
}

// NotificationBuilder 提供流畅的通知构建接口
type NotificationBuilder struct {
	notification *Notification // 待构建的通知
}

// NewNotificationBuilder 创建一个新的通知构建器
func NewNotificationBuilder() *NotificationBuilder {
	return &NotificationBuilder{
		notification: &Notification{
			Priority:  PriorityNormal, // 默认正常优先级
			Timestamp: time.Now(),     // 默认当前时间
		},
	}
}

// WithTopic 设置通知主题
func (b *NotificationBuilder) WithTopic(topic string) *NotificationBuilder {
	b.notification.Topic = topic
	return b
}

// WithType 设置通知类型
func (b *NotificationBuilder) WithType(notifType NotificationType) *NotificationBuilder {
	b.notification.Type = notifType
	return b
}

// WithPriority 设置通知优先级
func (b *NotificationBuilder) WithPriority(priority Priority) *NotificationBuilder {
	b.notification.Priority = priority
	return b
}

// WithPayload 设置通知内容
func (b *NotificationBuilder) WithPayload(payload interface{}) *NotificationBuilder {
	b.notification.Payload = payload
	return b
}

// WithExpiry 设置通知过期时间
func (b *NotificationBuilder) WithExpiry(expiry time.Time) *NotificationBuilder {
	b.notification.Expiry = expiry
	return b
}

// WithDestinations 设置特定接收者
func (b *NotificationBuilder) WithDestinations(destinations ...string) *NotificationBuilder {
	b.notification.Destinations = destinations
	return b
}

// Build 创建通知实例
func (b *NotificationBuilder) Build() *Notification {
	return b.notification
}

// MarshalJSON 自定义Notification的JSON序列化（时间格式化）
func (n *Notification) MarshalJSON() ([]byte, error) {
	type Alias Notification // 避免递归序列化
	return json.Marshal(&struct {
		*Alias
		Timestamp string `json:"timestamp"`  // 格式化的创建时间
		Expiry    string `json:"expiry,omitempty"` // 格式化的过期时间（可选）
		NextRetry string `json:"nextRetry,omitempty"` // 格式化的下次重试时间（可选）
	}{
		Alias:     (*Alias)(n),
		Timestamp: n.Timestamp.Format(time.RFC3339),
		Expiry:    n.Expiry.Format(time.RFC3339),
		NextRetry: n.NextRetry.Format(time.RFC3339),
	})
}

