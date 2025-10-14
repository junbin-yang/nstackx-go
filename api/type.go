// 公共API类型
package api

import (
	"net"
	"time"
)

// 发现模式
type DiscoveryMode uint8
const (
	ModeDefault   DiscoveryMode = 0
	ModeDiscover  DiscoveryMode = 1
	ModePublish   DiscoveryMode = 2
	ModeOffline   DiscoveryMode = 3
	ModeProactive DiscoveryMode = 10
)

// 发现类型
type DiscoveryType uint8
const (
	DiscoveryTypePassive DiscoveryType = 1
	DiscoveryTypeActive  DiscoveryType = 2
)

// 业务类型
type BusinessType uint8
const (
	BusinessTypeNull     BusinessType = 0
	BusinessTypeHicom    BusinessType = 1
	BusinessTypeSoftbus  BusinessType = 2
	BusinessTypeNearby   BusinessType = 3
	BusinessTypeAutonet  BusinessType = 4
	BusinessTypeStrategy BusinessType = 5
)

// 设备类型
type DeviceType uint32
const (
	DeviceTypeUnknown DeviceType = 0
	DeviceTypePhone   DeviceType = 1
	DeviceTypeTablet  DeviceType = 2
	DeviceTypePC      DeviceType = 3
	DeviceTypeTV      DeviceType = 4
	DeviceTypeCar     DeviceType = 5
	DeviceTypeWatch   DeviceType = 6
	DeviceTypeSpeaker DeviceType = 7
)

// 设备信息
type DeviceInfo struct {
	DeviceID           string
	DeviceName         string
	DeviceType         DeviceType
	CapabilityBitmap   []uint32
	Mode               uint8
	Update             bool
	NetworkName        string
	NetworkIP          net.IP
	DiscoveryType      DiscoveryType
	BusinessType       BusinessType
	ServiceData        string
	ExtendServiceData  string
	BusinessData       string
	LastSeenTime       time.Time
	DeviceHash         uint64
}

// network interface information
type InterfaceInfo struct {
	NetworkName string
	NetworkIP   net.IP
}

// local device information
type LocalDeviceInfo struct {
	Name              string
	DeviceID          string
	BTMacAddr         string
	WifiMacAddr       string
	LocalInterfaces   []InterfaceInfo
	Is5GHzSupported   bool
	BusinessType      BusinessType
	DeviceType        DeviceType
	DeviceHash        uint64
	HasDeviceHash     bool
}

// 发现配置
type DiscoverySettings struct {
	BusinessType       BusinessType
	DiscoveryMode      DiscoveryMode
	AdvertiseCount     uint32
	AdvertiseDuration  time.Duration
	AdvertiseInterval  time.Duration
	BusinessData       string
}

// 通知配置
type NotificationConfig struct {
	Message      string
	Intervals    []time.Duration
	BusinessType BusinessType
}

// NStackX的主要配置
type Config struct {
	LocalDevice      LocalDeviceInfo
	DiscoveryMode    DiscoveryMode
	MaxDeviceNum     uint32
	AgingTime        time.Duration
	LogLevel         string
	EnableStatistics bool
	EnableDump       bool
}

// 发现回掉
type Callbacks struct {
	OnDeviceListChanged    func(devices []DeviceInfo)
	OnDeviceFound          func(device *DeviceInfo)
	OnDeviceLost           func(deviceID string)
	OnMessageReceived      func(moduleName, deviceID string, data []byte, srcIP net.IP)
	OnNotificationReceived func(notification *NotificationConfig)
	OnError                func(err error)
}

// 运行时统计
type Statistics struct {
	DevicesDiscovered   uint64
	MessagesSent        uint64
	MessagesReceived    uint64
	DiscoveryRounds     uint64
	Errors              uint64
	LastDiscoveryTime   time.Time
	AverageResponseTime time.Duration
}

// 系统事件
type Event struct {
	Type      EventType
	Level     EventLevel
	Timestamp time.Time
	Message   string
	Data      map[string]interface{}
}

// 事件类型
type EventType string
const (
	EventTypeFault     EventType = "fault"
	EventTypeStatistic EventType = "statistic"
	EventTypeSecurity  EventType = "security"
	EventTypeBehavior  EventType = "behavior"
)

// 事件级别
type EventLevel string
const (
	EventLevelCritical EventLevel = "critical"
	EventLevelMinor    EventLevel = "minor"
	EventLevelInfo     EventLevel = "info"
)

// 服务接口
type Service interface {
	// 生命周期管理
	Init(config *Config) error
	Start() error
	Stop() error
	Destroy()

	// 设备管理
	RegisterDevice(device *LocalDeviceInfo) error
	UpdateDevice(device *LocalDeviceInfo) error
	GetDeviceList() ([]DeviceInfo, error)

	// 发现操作
	StartDiscovery(settings *DiscoverySettings) error
	StopDiscovery() error
	SendDiscoveryResponse(remoteIP net.IP, businessData string) error

	// 消息传递
	SendMessage(deviceID string, data []byte) error
	SendNotification(config *NotificationConfig) error
	StopNotification(businessType BusinessType) error

	// 能力管理
	RegisterCapability(capabilities []uint32) error
	SetFilterCapability(capabilities []uint32) error

	// 统计和监控
	GetStatistics() (*Statistics, error)
	DumpState() (string, error)

	// 回调注册
	RegisterCallbacks(callbacks *Callbacks)
}
