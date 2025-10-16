// Package service 实现设备发现的核心服务，整合CoAP通信、设备管理、网络管理等能力
package service

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/discovery/coap"   // 依赖CoAP协议模块
	"github.com/junbin-yang/nstackx-go/pkg/discovery/device" // 依赖设备管理模块
	"github.com/junbin-yang/nstackx-go/pkg/network"          // 依赖网络管理模块
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"     // 依赖日志工具
	"go.uber.org/zap"
)

// DiscoveryService 设备发现核心服务结构体，管理服务全生命周期与核心组件
type DiscoveryService struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问服务资源的安全性

	// Configuration 服务配置（从外部传入，含设备信息、发现模式、老化时间等）
	config *api.Config

	// Core components 核心组件（服务依赖的基础能力）
	deviceManager *device.Manager  // 设备管理器：维护本地/远程设备列表，清理过期设备
	coapServer    *coap.Server     // CoAP服务端：接收外部设备的CoAP消息（如设备响应）
	coapClient    *coap.Client     // CoAP客户端：发送CoAP广播/单播消息（如发现请求）
	networkMgr    *network.Manager // 网络管理器：监控网络接口状态，管理网络连接

	// Runtime state 服务运行状态
	ctx           context.Context    // 服务上下文，用于控制后台协程退出
	cancel        context.CancelFunc // 上下文取消函数，触发服务停止
	isRunning     bool               // 服务运行状态标记（true=运行中，false=已停止）
	discoveryMode api.DiscoveryMode  // 设备发现模式（如主动发现、被动响应等，来自配置）

	// Callbacks 外部回调函数：供外部模块感知设备状态变化（如设备新增/丢失）
	callbacks *api.Callbacks

	// Statistics 服务运行统计信息（如发现次数、接收消息数、错误数等）
	stats *api.Statistics

	// Logger 日志实例（基于zap框架，打印服务运行日志）
	log *logger.Logger
}

// NewDiscoveryService 创建设备发现服务实例，初始化配置与上下文
// 参数：config - 服务配置（含设备ID、发现模式、日志级别等）
// 返回：服务实例，若配置为nil或日志初始化失败则返回错误
func NewDiscoveryService(config *api.Config) (*DiscoveryService, error) {
	if config == nil {
		return nil, fmt.Errorf("配置不能为nil")
	}

	// 创建可取消上下文（用于控制服务生命周期）
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化服务核心字段
	s := &DiscoveryService{
		config:        config,
		ctx:           ctx,
		cancel:        cancel,
		discoveryMode: config.DiscoveryMode, // 初始发现模式来自配置
		stats:         &api.Statistics{},    // 初始化统计信息
		log:           logger.Default(),
		callbacks:     &api.Callbacks{}, // 初始化回调结构体（默认空实现）
	}

	// 初始化服务依赖的核心组件（设备管理器、网络管理器、CoAP服务端/客户端）
	if err := s.initComponents(); err != nil {
		cancel() // 组件初始化失败，取消上下文释放资源
		return nil, fmt.Errorf("初始化核心组件失败: %w", err)
	}

	return s, nil
}

// initComponents 初始化服务依赖的核心组件（设备、网络、CoAP相关）
// 返回：组件初始化失败则返回错误
func (s *DiscoveryService) initComponents() error {
	var err error

	// 1. 初始化设备管理器：传入最大设备数、设备老化时间（超时未更新则清理）
	s.deviceManager = device.NewManager(s.config.MaxDeviceNum, s.config.AgingTime)

	// 2. 初始化网络管理器：管理网络接口、监控网络状态
	s.networkMgr, err = network.NewManager()
	if err != nil {
		return fmt.Errorf("创建网络管理器失败: %w", err)
	}

	// 3. 初始化CoAP服务端：绑定消息处理函数（接收消息后调用handleCoapMessage）
	s.coapServer, err = coap.NewServer(s.handleCoapMessage)
	if err != nil {
		return fmt.Errorf("创建CoAP服务端失败: %w", err)
	}

	// 4. 初始化CoAP客户端：用于发送发现广播/单播消息
	s.coapClient = coap.NewClient(s.networkMgr)

	return nil
}

// Init 重新初始化服务配置（需在服务未运行时调用）
// 参数：config - 新的服务配置
// 返回：若服务已运行则返回错误
func (s *DiscoveryService) Init(config *api.Config) error {
	s.mu.Lock()         // 写锁：保障配置更新时的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 服务运行中不允许重新初始化
	if s.isRunning {
		return fmt.Errorf("服务已运行，无法重新初始化")
	}

	// 更新服务配置
	s.config = config
	s.log.Info("初始化设备发现服务",
		zap.String("设备ID", config.LocalDevice.DeviceID),
		zap.String("设备名称", config.LocalDevice.Name))

	return nil
}

// Start 启动设备发现服务（启动核心组件与后台协程）
// 返回：若组件启动失败则返回错误（如CoAP服务端启动失败）
func (s *DiscoveryService) Start() error {
	s.mu.Lock()         // 写锁：保障服务启动过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 服务已运行则直接返回错误
	if s.isRunning {
		return fmt.Errorf("服务已运行")
	}

	s.log.Info("启动设备发现服务")

	// 1. 启动CoAP服务端：开始监听CoAP请求（默认端口5683）
	if err := s.coapServer.Start(); err != nil {
		return fmt.Errorf("启动CoAP服务端失败: %w", err)
	}

	// 2. 启动网络管理器：开始监控网络接口状态
	if err := s.networkMgr.Start(); err != nil {
		s.coapServer.Stop() // 网络管理器启动失败，回滚CoAP服务端
		return fmt.Errorf("启动网络管理器失败: %w", err)
	}

	// 标记服务为运行状态
	s.isRunning = true

	// 3. 启动后台协程：周期性执行发现任务与设备清理任务
	go s.discoveryWorker() // 周期性设备发现（如每5秒一次）
	go s.cleanupWorker()   // 周期性清理过期设备（如每10秒一次）

	s.log.Info("设备发现服务启动成功")
	return nil
}

// Stop 停止设备发现服务（停止组件与后台协程）
// 返回：若服务未运行则返回错误
func (s *DiscoveryService) Stop() error {
	s.mu.Lock()         // 写锁：保障服务停止过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 服务未运行则直接返回错误
	if !s.isRunning {
		return fmt.Errorf("服务未运行")
	}

	s.log.Info("停止设备发现服务")

	// 1. 取消上下文：通知所有后台协程（discoveryWorker/cleanupWorker）退出
	s.cancel()

	// 2. 停止核心组件
	s.coapServer.Stop() // 停止CoAP服务端（关闭监听）
	s.networkMgr.Stop() // 停止网络管理器（停止网络监控）

	// 标记服务为停止状态
	s.isRunning = false

	s.log.Info("设备发现服务已停止")
	return nil
}

// Destroy 彻底销毁服务，释放所有资源（需在服务停止后调用）
func (s *DiscoveryService) Destroy() {
	s.mu.Lock()         // 写锁：保障资源清理的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 若服务仍在运行，先停止服务
	if s.isRunning {
		s.Stop()
	}

	// 释放所有核心组件资源（置为nil，便于GC回收）
	s.deviceManager = nil
	s.coapServer = nil
	s.coapClient = nil
	s.networkMgr = nil

	s.log.Info("设备发现服务已彻底销毁（资源已释放）")
}

// RegisterDevice 注册本地设备信息（将本地设备加入管理）
// 参数：device - 本地设备信息（含设备ID、名称、网络信息等）
// 返回：若设备信息为nil则返回错误
func (s *DiscoveryService) RegisterDevice(device *api.LocalDeviceInfo) error {
	s.mu.Lock()         // 写锁：保障设备信息更新的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	if device == nil {
		return fmt.Errorf("设备信息不能为nil")
	}

	// 更新服务配置中的本地设备信息
	s.config.LocalDevice = *device
	s.log.Info("本地设备注册成功",
		zap.String("设备ID", device.DeviceID),
		zap.String("设备名称", device.Name))

	return nil
}

// UpdateDevice 更新本地设备信息（复用RegisterDevice逻辑，本质是覆盖配置）
// 参数：device - 新的本地设备信息
// 返回：同RegisterDevice
func (s *DiscoveryService) UpdateDevice(device *api.LocalDeviceInfo) error {
	return s.RegisterDevice(device)
}

// GetDeviceList 获取当前已发现的所有设备列表
// 返回：设备列表，若设备管理器未初始化则返回错误
func (s *DiscoveryService) GetDeviceList() ([]api.DeviceInfo, error) {
	s.mu.RLock()         // 读锁：仅读取设备列表，不修改
	defer s.mu.RUnlock() // 函数退出时释放锁

	// 设备管理器未初始化（异常场景）
	if s.deviceManager == nil {
		return nil, fmt.Errorf("设备管理器未初始化")
	}

	// 从设备管理器获取所有设备
	return s.deviceManager.GetAllDevices(), nil
}

// StartDiscovery 手动触发设备发现（按传入的设置执行）
// 参数：settings - 发现设置（如发现模式、广播次数）
// 返回：若服务未运行则返回错误
func (s *DiscoveryService) StartDiscovery(settings *api.DiscoverySettings) error {
	s.mu.Lock()         // 写锁：保障发现模式更新的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	if !s.isRunning {
		return fmt.Errorf("服务未运行，无法启动设备发现")
	}

	s.log.Info("手动启动设备发现",
		zap.String("发现模式", fmt.Sprintf("%v", settings.DiscoveryMode)),
		zap.Uint32("广播次数", settings.AdvertiseCount))

	// 更新服务的发现模式（覆盖当前模式）
	s.discoveryMode = settings.DiscoveryMode

	// 启动协程发送发现消息（非阻塞，避免阻塞当前函数）
	go s.sendDiscoveryMessages(settings)

	return nil
}

// StopDiscovery 停止设备发现（重置发现模式为默认）
// 返回：无实际错误（仅状态更新）
func (s *DiscoveryService) StopDiscovery() error {
	s.mu.Lock()         // 写锁：保障发现模式更新的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	s.log.Info("停止设备发现")
	// 重置发现模式为默认（ModeDefault），后台discoveryWorker会停止周期性发现
	s.discoveryMode = api.ModeDefault

	return nil
}

// SendMessage 向指定远程设备发送消息（基于设备ID）
// 参数：deviceID - 目标设备ID；data - 待发送的消息数据
// 返回：若设备未找到或发送失败则返回错误
func (s *DiscoveryService) SendMessage(deviceID string, data []byte) error {
	s.mu.RLock()         // 读锁：仅读取设备信息，不修改
	defer s.mu.RUnlock() // 函数退出时释放锁

	// 从设备管理器获取目标设备（按设备ID）
	device := s.deviceManager.GetDevice(deviceID)
	if device == nil {
		return fmt.Errorf("未找到设备: %s", deviceID)
	}

	s.log.Debug("向设备发送消息",
		zap.String("设备ID", deviceID),
		zap.Int("消息长度", len(data)))

	// 通过CoAP客户端向设备的网络IP发送消息（使用默认CoAP端口）
	return s.coapClient.SendMessage(device.NetworkIP, data)
}

// RegisterCallbacks 注册外部回调函数（用于感知设备状态变化）
// 参数：callbacks - 外部实现的回调函数（如OnDeviceFound/OnDeviceLost）
func (s *DiscoveryService) RegisterCallbacks(callbacks *api.Callbacks) {
	s.mu.Lock()         // 写锁：保障回调函数更新的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	s.callbacks = callbacks
	s.log.Info("外部回调函数注册成功")
}

// GetStatistics 获取服务运行统计信息（如发现次数、接收消息数等）
// 返回：统计信息实例，无错误（仅读取）
func (s *DiscoveryService) GetStatistics() (*api.Statistics, error) {
	s.mu.RLock()         // 读锁：仅读取统计信息，不修改
	defer s.mu.RUnlock() // 函数退出时释放锁

	return s.stats, nil
}

// discoveryWorker 后台协程：周期性执行设备发现（默认每5秒一次）
// 逻辑：若发现模式非默认（ModeDefault），则执行一次发现任务
func (s *DiscoveryService) discoveryWorker() {
	// 创建定时器：每5秒触发一次
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop() // 协程退出时停止定时器

	for {
		select {
		case <-s.ctx.Done(): // 服务停止（上下文取消），退出协程
			return
		case <-ticker.C: // 定时器触发，检查是否需要执行发现
			// 仅当发现模式非默认时，执行发现任务
			if s.discoveryMode != api.ModeDefault {
				s.performDiscovery()
			}
		}
	}
}

// cleanupWorker 后台协程：周期性清理过期设备（默认每10秒一次）
// 逻辑：调用设备管理器清理超时未更新的设备，触发设备丢失回调
func (s *DiscoveryService) cleanupWorker() {
	// 创建定时器：每10秒触发一次
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop() // 协程退出时停止定时器

	for {
		select {
		case <-s.ctx.Done(): // 服务停止，退出协程
			return
		case <-ticker.C: // 定时器触发，执行设备清理
			s.cleanupExpiredDevices()
		}
	}
}

// performDiscovery 执行一次设备发现任务（发送CoAP广播）
// 逻辑：构建发现消息，通过CoAP客户端发送广播，更新统计信息
func (s *DiscoveryService) performDiscovery() {
	s.mu.Lock()         // 写锁：保障发现过程中资源的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	s.log.Debug("执行一次设备发现")
	// 更新统计信息：发现轮次+1，记录最后发现时间
	s.stats.DiscoveryRounds++
	s.stats.LastDiscoveryTime = time.Now()

	// 1. 构建发现消息（待实现：按协议格式封装本地设备信息）
	msg := s.buildDiscoveryMessage()
	// 2. 通过CoAP客户端发送广播消息（向所有支持组播的网络接口发送）
	if err := s.coapClient.SendBroadcast(msg); err != nil {
		s.log.Error("发送发现广播失败", zap.Error(err))
		s.stats.Errors++ // 统计错误数+1
	}
}

// cleanupExpiredDevices 清理过期设备（调用设备管理器）并触发回调
// 逻辑：获取过期设备ID列表，触发OnDeviceLost回调（若注册）
func (s *DiscoveryService) cleanupExpiredDevices() {
	s.mu.Lock()         // 写锁：保障设备清理过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 调用设备管理器清理过期设备，返回过期设备ID列表
	expiredDeviceIDs := s.deviceManager.CleanupExpired()
	for _, deviceID := range expiredDeviceIDs {
		s.log.Debug("设备已过期（清理）", zap.String("设备ID", deviceID))
		// 若注册了设备丢失回调，触发回调（协程执行，避免阻塞）
		if s.callbacks.OnDeviceLost != nil {
			go s.callbacks.OnDeviceLost(deviceID)
		}
	}
}

// handleCoapMessage CoAP消息处理函数（CoAP服务端的回调）
// 逻辑：解析消息中的设备信息，更新设备列表，触发相关回调
func (s *DiscoveryService) handleCoapMessage(msg *coap.Message) {
	s.mu.Lock()         // 写锁：保障消息处理过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 更新统计信息：接收消息数+1
	s.stats.MessagesReceived++

	// 1. 从CoAP消息中解析设备信息（待实现：按协议格式解析payload）
	deviceInfo, err := s.parseDeviceInfo(msg)
	if err != nil {
		s.log.Error("解析设备信息失败", zap.Error(err))
		s.stats.Errors++ // 统计错误数+1
		return
	}

	// 2. 更新设备管理器中的设备：返回是否为新设备（首次发现）
	isNewDevice := s.deviceManager.UpdateDevice(deviceInfo)

	// 3. 触发回调：根据设备状态执行不同回调
	if isNewDevice {
		// 新设备：统计已发现设备数+1，触发设备新增回调
		s.stats.DevicesDiscovered++
		if s.callbacks.OnDeviceFound != nil {
			go s.callbacks.OnDeviceFound(deviceInfo) // 协程执行，避免阻塞
		}
	}

	// 触发设备列表变更回调（无论是否新设备，列表均可能更新）
	if s.callbacks.OnDeviceListChanged != nil {
		// 获取最新设备列表
		allDevices := s.deviceManager.GetAllDevices()
		go s.callbacks.OnDeviceListChanged(allDevices) // 协程执行
	}
}

// buildDiscoveryMessage 构建设备发现消息（待实现）
// 功能：按协议格式封装本地设备信息（如设备ID、能力、网络信息）
// 返回：待发送的消息字节流（当前返回空，需后续实现）
func (s *DiscoveryService) buildDiscoveryMessage() []byte {
	// TODO: 待实现：根据协议规范构建发现消息（如JSON格式的设备信息）
	return []byte{}
}

// parseDeviceInfo 从CoAP消息中解析设备信息（待实现）
// 参数：msg - 接收的CoAP消息（含设备发送的payload）
// 返回：解析后的设备信息，解析失败则返回错误
func (s *DiscoveryService) parseDeviceInfo(msg *coap.Message) (*api.DeviceInfo, error) {
	// TODO: 待实现：从消息payload中解析设备信息（如设备ID、IP、能力等）
	return &api.DeviceInfo{}, nil
}

// sendDiscoveryMessages 根据设置发送多次发现消息（待实现）
// 参数：settings - 发现设置（如广播次数、间隔等）
func (s *DiscoveryService) sendDiscoveryMessages(settings *api.DiscoverySettings) {
	// TODO: 待实现：按settings.AdvertiseCount发送指定次数的发现消息，支持间隔控制
}

// ------------------------------ 待实现的接口方法 ------------------------------
// SendDiscoveryResponse 向远程设备发送发现响应（如收到发现请求后回复）
// 参数：remoteIP - 远程设备IP；businessData - 业务数据（如本地设备信息）
// 返回：待实现
func (s *DiscoveryService) SendDiscoveryResponse(remoteIP net.IP, businessData string) error {
	// TODO: 待实现：构建CoAP响应消息，向remoteIP发送
	return nil
}

// SendNotification 向设备发送通知（如设备状态变更通知）
// 参数：config - 通知配置（含目标设备、通知类型、内容等）
// 返回：待实现
func (s *DiscoveryService) SendNotification(config *api.NotificationConfig) error {
	// TODO: 待实现：按通知配置构建消息，通过CoAP客户端发送
	return nil
}

// StopNotification 停止发送指定类型的通知
// 参数：businessType - 业务类型（如设备在线状态通知）
// 返回：待实现
func (s *DiscoveryService) StopNotification(businessType api.BusinessType) error {
	// TODO: 待实现：停止对应业务类型的通知发送（如取消定时器）
	return nil
}

// RegisterCapability 注册本地设备的能力（如支持的功能模块）
// 参数：capabilities - 能力ID列表
// 返回：待实现
func (s *DiscoveryService) RegisterCapability(capabilities []uint32) error {
	// TODO: 待实现：将能力信息存入本地设备配置，用于发现消息广播
	return nil
}

// SetFilterCapability 设置设备发现的能力过滤（仅发现具备指定能力的设备）
// 参数：capabilities - 目标能力ID列表
// 返回：待实现
func (s *DiscoveryService) SetFilterCapability(capabilities []uint32) error {
	// TODO: 待实现：更新设备管理器的过滤规则，清理不满足能力的设备
	return nil
}

// DumpState 导出服务当前运行状态（用于调试，如设备列表、统计信息）
// 返回：状态字符串，待实现
func (s *DiscoveryService) DumpState() (string, error) {
	// TODO: 待实现：拼接设备列表、统计信息、组件状态为字符串
	return "", nil
}
