package notification

import (
	"errors"
	"testing"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 测试基本的订阅与发布功能
func TestBasicSubscriptionAndPublish(t *testing.T) {
	// 配置一个小队列大小便于测试
	config := &Config{
		MaxQueueSize:  10,
		MaxRetries:    2,
		RetryInterval: 100 * time.Millisecond,
	}
	nm := NewNotificationManager(config)
	defer nm.Stop()

	// 用于记录接收到的通知
	var received []*Notification
	callback := func(n *Notification) error {
		received = append(received, n)
		return nil
	}

	// 订阅主题
	subID, err := nm.Subscribe("discovery.discover", callback)
	require.NoError(t, err)
	require.NotEmpty(t, subID)

	// 发布设备发现通知（会创建"discovery.discover"主题的通知）
	device := &api.DeviceInfo{DeviceID: "device-123", DeviceName: "TestDevice"}
	err = nm.PublishDiscovery("discover", device)
	require.NoError(t, err)

	// 等待通知处理
	time.Sleep(300 * time.Millisecond)

	// 验证结果
	assert.Len(t, received, 1, "应收到1条通知")
	assert.Equal(t, NotificationTypeDiscovery, received[0].Type)
	assert.Equal(t, "discovery.discover", received[0].Topic)
	
	payload, ok := received[0].Payload.(*api.DeviceInfo)
	assert.True(t, ok, "payload应该是DeviceInfo类型")
	assert.Equal(t, "device-123", payload.DeviceID)
}

// 测试带过滤器的订阅
func TestFilteredSubscription(t *testing.T) {
	nm := NewNotificationManager(nil)
	defer nm.Stop()

	var highPriorityReceived int
	// 只接收高优先级通知的过滤器
	filter := func(n *Notification) bool {
		return n.Priority == PriorityHigh
	}

	// 创建带过滤器的订阅
	_, err := nm.SubscribeWithFilter("status", func(n *Notification) error {
		highPriorityReceived++
		return nil
	}, filter)
	require.NoError(t, err)

	// 发布不同优先级的通知
	nm.Publish(&Notification{
		Topic:    "status",
		Priority: PriorityLow,
	})
	nm.Publish(&Notification{
		Topic:    "status",
		Priority: PriorityHigh,
	})
	nm.Publish(&Notification{
		Topic:    "status",
		Priority: PriorityCritical,
	})

	time.Sleep(100 * time.Millisecond)

	// 验证只有高优先级通知被接收
	assert.Equal(t, 1, highPriorityReceived)
}

// 测试广播功能
func TestBroadcast(t *testing.T) {
	nm := NewNotificationManager(nil)
	defer nm.Stop()

	received1 := false
	received2 := false

	// 两个不同主题的订阅者
	_, _ = nm.Subscribe("device", func(n *Notification) error {
		received1 = true
		return nil
	})
	_, _ = nm.Subscribe("status", func(n *Notification) error {
		received2 = true
		return nil
	})

	// 发送广播
	err := nm.Broadcast(&Notification{Type: NotificationTypeCustom})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	// 验证所有订阅者都收到广播
	assert.True(t, received1)
	assert.True(t, received2)
}

// 测试重试机制
func TestRetryMechanism(t *testing.T) {
	config := &Config{
		MaxRetries:    2,
		RetryInterval: 100 * time.Millisecond,
	}
	nm := NewNotificationManager(config)
	defer nm.Stop()

	attempts := 0
	// 前两次失败，第三次成功的回调
	callback := func(n *Notification) error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary error")
		}
		return nil
	}

	_, err := nm.Subscribe("test.retry", callback)
	require.NoError(t, err)

	// 发布通知
	err = nm.Publish(&Notification{Topic: "test.retry"})
	require.NoError(t, err)

	// 等待重试完成
	time.Sleep(500 * time.Millisecond)

	// 验证重试次数（初始1次 + 2次重试 = 3次）
	assert.Equal(t, 3, attempts)

	// 验证统计信息
	stats := nm.GetStatistics()
	assert.Equal(t, uint64(1), stats.TotalSent)
	assert.Equal(t, uint64(2), stats.TotalFailed) // 前两次失败
	assert.Equal(t, uint64(2), stats.TotalRetried)
}

// 测试队列满的情况
func TestQueueFull(t *testing.T) {
	// 队列大小为2的配置
	config := &Config{MaxQueueSize: 2}
	nm := NewNotificationManager(config)
	defer nm.Stop()

	// 填满队列
	n1 := &Notification{Topic: "test"}
	n2 := &Notification{Topic: "test"}
	n3 := &Notification{Topic: "test"}

	assert.NoError(t, nm.Publish(n1))
	assert.NoError(t, nm.Publish(n2))
	assert.Error(t, nm.Publish(n3)) // 第三次应该失败

	// 验证统计信息
	stats := nm.GetStatistics()
	assert.Equal(t, uint64(1), stats.TotalDropped)
}

// 测试设备通知发布
func TestPublishDevice(t *testing.T) {
	nm := NewNotificationManager(nil)
	defer nm.Stop()

	var receivedDevice *api.DeviceInfo
	_, _ = nm.Subscribe("device.connected", func(n *Notification) error {
		payload, ok := n.Payload.(map[string]interface{})
		if ok {
			receivedDevice, _ = payload["device"].(*api.DeviceInfo)
		}
		return nil
	})

	device := &api.DeviceInfo{DeviceID: "dev-456", DeviceName: "TestDevice456"}
	err := nm.PublishDevice(device, "connected")
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	assert.NotNil(t, receivedDevice)
	assert.Equal(t, "dev-456", receivedDevice.DeviceID)
}

// 测试统计信息准确性
func TestStatistics(t *testing.T) {
	nm := NewNotificationManager(nil)
	defer nm.Stop()

	// 创建2个订阅者
	_, _ = nm.Subscribe("test.stats", func(n *Notification) error { return nil })
	_, _ = nm.Subscribe("test.stats2", func(n *Notification) error { return nil })

	// 发布3个通知
	_ = nm.Publish(&Notification{Topic: "test.stats"})
	_ = nm.Publish(&Notification{Topic: "test.stats"})
	_ = nm.Publish(&Notification{Topic: "test.stats2"})

	time.Sleep(100 * time.Millisecond)

	stats := nm.GetStatistics()
	assert.Equal(t, uint64(3), stats.TotalSent)
	assert.Equal(t, 2, stats.SubscriberCount)
	assert.Equal(t, 2, stats.TopicCount)
}

