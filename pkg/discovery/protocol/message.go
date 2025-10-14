// 定义设备发现协议的消息结构及相关处理方法
package protocol

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
)

// MessageType 表示发现协议消息的类型
type MessageType string

const (
	MessageTypeDiscover     MessageType = "discover"     // 发现消息：主动探测网络中的设备
	MessageTypeResponse     MessageType = "response"     // 响应消息：对发现消息的回复
	MessageTypeNotification MessageType = "notification" // 通知消息：设备状态变更等通知
	MessageTypeHeartbeat    MessageType = "heartbeat"    // 心跳消息：维持设备在线状态
)

// Message 表示发现协议的消息结构，包含协议交互所需的所有信息
type Message struct {
	Version      string                 `json:"version"`      // 协议版本（如"1.0"）
	Type         MessageType            `json:"type"`         // 消息类型（对应MessageType常量）
	Timestamp    int64                  `json:"timestamp"`    // 消息发送时间戳（Unix时间，秒级）
	DeviceInfo   *DeviceInfo            `json:"deviceInfo,omitempty"` // 设备信息（可选，根据消息类型决定是否必填）
	Capability   []uint32               `json:"capability,omitempty"` // 设备能力列表（可选，标识设备支持的功能）
	ServiceData  string                 `json:"serviceData,omitempty"` // 服务相关数据（可选）
	BusinessData string                 `json:"businessData,omitempty"` // 业务自定义数据（可选）
	Extra        map[string]interface{} `json:"extra,omitempty"` // 额外扩展字段（可选，用于兼容未来需求）
}

// DeviceInfo 表示消息中包含的设备信息，用于标识设备身份和网络属性
type DeviceInfo struct {
	DeviceID      string `json:"deviceId"`      // 设备唯一标识（必填）
	DeviceName    string `json:"deviceName"`    // 设备名称（可选，用于展示）
	DeviceType    uint32 `json:"deviceType"`    // 设备类型（如摄像头、传感器等，对应业务定义）
	DeviceHash    uint64 `json:"deviceHash,omitempty"` // 设备哈希值（可选，用于快速校验设备唯一性）
	NetworkName   string `json:"networkName,omitempty"` // 网络名称（可选，如设备所在WiFi名称）
	NetworkIP     string `json:"networkIp,omitempty"`   // 设备网络IP地址（可选，用于直接通信）
	BTMacAddr     string `json:"btMacAddr,omitempty"`   // 蓝牙MAC地址（可选，支持蓝牙的设备）
	WifiMacAddr   string `json:"wifiMacAddr,omitempty"` // WiFi MAC地址（可选，支持WiFi的设备）
	BusinessType  uint8  `json:"businessType,omitempty"` // 业务类型（可选，区分不同业务场景）
	DiscoveryType uint8  `json:"discoveryType,omitempty"` // 发现方式（可选，如主动发现、被动广播等）
}

// NewDiscoverMessage 创建一个新的发现消息（用于主动探测网络中的其他设备）
// 参数localDevice：本地设备信息，用于填充消息中的设备标识
func NewDiscoverMessage(localDevice *api.LocalDeviceInfo) *Message {
	return &Message{
		Version:   "1.0",
		Type:      MessageTypeDiscover,
		Timestamp: time.Now().Unix(),
		DeviceInfo: &DeviceInfo{
			DeviceID:     localDevice.DeviceID,
			DeviceName:   localDevice.Name,
			DeviceType:   uint32(localDevice.DeviceType),
			DeviceHash:   localDevice.DeviceHash,
			BTMacAddr:    localDevice.BTMacAddr,
			WifiMacAddr:  localDevice.WifiMacAddr,
			BusinessType: uint8(localDevice.BusinessType),
		},
	}
}

// NewResponseMessage 创建一个新的响应消息（用于回复收到的发现消息）
// 参数localDevice：本地设备信息；businessData：业务自定义响应数据
func NewResponseMessage(localDevice *api.LocalDeviceInfo, businessData string) *Message {
	msg := NewDiscoverMessage(localDevice)
	msg.Type = MessageTypeResponse       // 覆盖消息类型为响应
	msg.BusinessData = businessData      // 填充业务响应数据
	return msg
}

// NewNotificationMessage 创建一个新的通知消息（用于广播设备状态变更等事件）
// 参数notification：通知内容
func NewNotificationMessage(notification string) *Message {
	return &Message{
		Version:   "1.0",
		Type:      MessageTypeNotification,
		Timestamp: time.Now().Unix(),
		Extra: map[string]interface{}{
			"notification": notification, // 通知内容存储在扩展字段中
		},
	}
}

// Encode 将消息编码为JSON字节流（用于网络传输）
func (m *Message) Encode() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("消息编码失败: %w", err)
	}
	return data, nil
}

// Decode 将JSON字节流解码为Message对象（用于解析收到的网络数据）
func Decode(data []byte) (*Message, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("消息解码失败: %w", err)
	}
	return &msg, nil
}

// ToDeviceInfo 将协议消息中的DeviceInfo转换为API层的DeviceInfo（用于上层业务处理）
func (m *Message) ToDeviceInfo() (*api.DeviceInfo, error) {
	if m.DeviceInfo == nil {
		return nil, fmt.Errorf("消息中不包含设备信息")
	}

	// 解析网络IP地址
	ip := net.ParseIP(m.DeviceInfo.NetworkIP)
	if ip == nil && m.DeviceInfo.NetworkIP != "" {
		return nil, fmt.Errorf("无效的IP地址: %s", m.DeviceInfo.NetworkIP)
	}

	return &api.DeviceInfo{
		DeviceID:       m.DeviceInfo.DeviceID,
		DeviceName:     m.DeviceInfo.DeviceName,
		DeviceType:     api.DeviceType(m.DeviceInfo.DeviceType),
		NetworkName:    m.DeviceInfo.NetworkName,
		NetworkIP:      ip,
		BusinessType:   api.BusinessType(m.DeviceInfo.BusinessType),
		DiscoveryType:  api.DiscoveryType(m.DeviceInfo.DiscoveryType),
		ServiceData:    m.ServiceData,
		BusinessData:   m.BusinessData,
		DeviceHash:     m.DeviceInfo.DeviceHash,
		LastSeenTime:   time.Now(), // 记录当前时间为最后发现时间
	}, nil
}

// Validate 验证消息的合法性（检查必填字段是否存在）
func (m *Message) Validate() error {
	if m.Version == "" {
		return fmt.Errorf("缺少协议版本")
	}
	if m.Type == "" {
		return fmt.Errorf("缺少消息类型")
	}
	// 发现消息和响应消息必须包含设备信息
	if m.Type == MessageTypeDiscover || m.Type == MessageTypeResponse {
		if m.DeviceInfo == nil {
			return fmt.Errorf("缺少设备信息")
		}
		if m.DeviceInfo.DeviceID == "" {
			return fmt.Errorf("缺少设备唯一标识（DeviceID）")
		}
	}
	return nil
}

// IsExpired 检查消息是否过期（超过最大存活时间）
// 参数maxAge：消息的最大存活时间（如5秒）
func (m *Message) IsExpired(maxAge time.Duration) bool {
	messageTime := time.Unix(m.Timestamp, 0) // 将时间戳转换为time.Time
	return time.Since(messageTime) > maxAge   // 若当前时间与消息时间差超过maxAge，则视为过期
}

// Clone 创建消息的深拷贝（避免修改原消息时影响其他引用）
func (m *Message) Clone() *Message {
	data, _ := m.Encode()   // 先编码为JSON
	clone, _ := Decode(data) // 再解码为新对象，实现深拷贝
	return clone
}

