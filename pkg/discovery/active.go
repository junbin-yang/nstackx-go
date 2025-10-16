// 实现主动设备发现功能，基于CoAP组播协议发送发现请求、接收响应并解析设备信息
package discovery

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/discovery/coap"
	"github.com/junbin-yang/nstackx-go/pkg/discovery/protocol"
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// defaultDiscoveryIntervals 默认发现间隔（毫秒级），采用渐进式间隔减少网络占用
// 逻辑：前期间隔短（快速发现设备），后期间隔长（减少重复发送）
var defaultDiscoveryIntervals = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	200 * time.Millisecond,
	300 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	5 * time.Second,
	10 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

// ActiveDiscovery 主动设备发现核心结构体，管理发现流程、组播通信与消息处理
type ActiveDiscovery struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问时的资源安全

	// Configuration 配置相关字段（初始化时传入或默认生成）
	localDevice *api.LocalDeviceInfo   // 本地设备信息（用于构建发现消息）
	settings    *api.DiscoverySettings // 发现配置（如广播次数、间隔、业务数据）
	
	// Network components 网络通信组件
	multicast *coap.MulticastHandler // CoAP组播处理器（发送广播、接收响应）
	encoder   *coap.Encoder           // CoAP消息编解码器（协议层消息处理）
	
	// Discovery state 发现流程状态
	isRunning     bool   // 发现服务运行状态（true=运行中）
	currentRound  int    // 当前发现轮次（从0开始）
	totalRounds   int    // 总发现轮次（由间隔列表长度决定）
	intervals     []time.Duration // 每轮发现的间隔（渐进式或固定）
	
	// Control 流程控制组件
	ctx    context.Context    // 上下文，用于控制协程退出
	cancel context.CancelFunc // 上下文取消函数，触发发现停止
	wg     sync.WaitGroup     // 等待组，确保停止时所有协程完成
	
	// Callbacks 外部回调函数（通知外部设备发现结果或错误）
	onDeviceFound func(*api.DeviceInfo) // 设备发现回调（传入新发现的设备信息）
	onError       func(error)          // 错误回调（传入发现过程中的错误）
	
	// Statistics 发现统计信息
	messagesSent uint64 // 已发送的发现消息总数
	roundsCount  uint64 // 已完成的发现轮次总数
	
	log *logger.Logger // 日志实例（基于zap框架，打印发现过程日志）
}

// ActiveDiscoveryConfig 主动发现的初始化配置结构体，聚合所需的外部依赖与回调
type ActiveDiscoveryConfig struct {
	LocalDevice   *api.LocalDeviceInfo   // 本地设备信息（必填，用于标识自身）
	Settings      *api.DiscoverySettings // 发现配置（可选，自定义广播规则）
	Multicast     *coap.MulticastHandler // CoAP组播处理器（可选，外部传入或内部默认创建）
	OnDeviceFound func(*api.DeviceInfo) // 设备发现回调（可选，通知外部新设备）
	OnError       func(error)          // 错误回调（可选，通知外部错误）
}

// NewActiveDiscovery 创建主动设备发现实例，初始化配置、上下文与核心组件
// 参数：config - 初始化配置（LocalDevice必填，其他可选）
// 返回：发现实例，若配置无效（如LocalDevice为nil）则返回错误
func NewActiveDiscovery(config *ActiveDiscoveryConfig) (*ActiveDiscovery, error) {
	// 校验核心配置：config与LocalDevice不能为nil
	if config == nil || config.LocalDevice == nil {
		return nil, fmt.Errorf("无效配置（config或LocalDevice不能为空）")
	}

	// 创建可取消上下文，用于控制发现服务生命周期
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化主动发现核心字段
	ad := &ActiveDiscovery{
		localDevice:   config.LocalDevice,
		settings:      config.Settings,
		multicast:     config.Multicast,
		encoder:       coap.NewEncoder(), // 创建CoAP编解码器实例
		onDeviceFound: config.OnDeviceFound,
		onError:       config.OnError,
		ctx:           ctx,
		cancel:        cancel,
		log:           logger.Default(),
	}

	// 初始化发现间隔（根据配置生成或使用默认）
	ad.initializeIntervals()

	return ad, nil
}

// initializeIntervals 根据配置生成发现间隔列表，支持三种模式：自定义间隔、固定间隔、默认间隔
func (ad *ActiveDiscovery) initializeIntervals() {
	ad.mu.Lock()         // 写锁：保障间隔列表初始化的线程安全
	defer ad.mu.Unlock() // 函数退出时释放锁

	// 若未配置Settings，直接使用默认间隔
	if ad.settings == nil {
		ad.intervals = defaultDiscoveryIntervals
		ad.totalRounds = len(defaultDiscoveryIntervals)
		return
	}

	// 模式1：自定义广播次数+总时长 → 计算平均间隔并添加抖动（避免设备同步发送）
	if ad.settings.AdvertiseCount > 0 && ad.settings.AdvertiseDuration > 0 {
		count := int(ad.settings.AdvertiseCount) // 广播次数
		baseInterval := ad.settings.AdvertiseDuration / time.Duration(count) // 基础间隔

		ad.intervals = make([]time.Duration, count)
		for i := 0; i < count; i++ {
			// 添加10%以内的随机抖动（避免多个设备同时发送导致网络冲突）
			jitter := time.Duration(rand.Int63n(int64(baseInterval / 10)))
			ad.intervals[i] = baseInterval + jitter
		}
		ad.totalRounds = count
	} 
	// 模式2：固定间隔（可选指定次数）→ 所有轮次使用相同间隔
	else if ad.settings.AdvertiseInterval > 0 {
		count := 10 // 默认广播10次
		if ad.settings.AdvertiseCount > 0 { // 若配置了次数则覆盖默认
			count = int(ad.settings.AdvertiseCount)
		}

		ad.intervals = make([]time.Duration, count)
		for i := 0; i < count; i++ {
			ad.intervals[i] = ad.settings.AdvertiseInterval
		}
		ad.totalRounds = count
	} 
	// 模式3：未配置有效间隔/次数 → 使用默认渐进式间隔
	else {
		ad.intervals = defaultDiscoveryIntervals
		ad.totalRounds = len(defaultDiscoveryIntervals)
	}

	// 打印初始化结果日志
	ad.log.Info("发现间隔初始化完成",
		zap.Int("总轮次", ad.totalRounds),
		zap.Duration("第一轮间隔", ad.intervals[0]),
		zap.Duration("最后一轮间隔", ad.intervals[ad.totalRounds-1]))
}

// Start 启动主动设备发现服务，初始化组播处理器（若未提供）并启动核心协程
// 返回：若服务已运行或组播处理器创建失败则返回错误
func (ad *ActiveDiscovery) Start() error {
	ad.mu.Lock()         // 写锁：保障启动过程的线程安全
	defer ad.mu.Unlock() // 函数退出时释放锁

	// 若服务已运行，直接返回错误
	if ad.isRunning {
		return fmt.Errorf("主动发现服务已运行")
	}

	// 若未提供外部组播处理器，创建默认组播配置并初始化
	if ad.multicast == nil {
		multicastConfig := &coap.MulticastConfig{
			IPv4Enabled: true,  // 启用IPv4组播
			IPv6Enabled: true,  // 启用IPv6组播
			Port:        coap.CoAPPort, // 使用CoAP默认端口5683
			TTL:         coap.DefaultTTL, // 默认TTL=255（最大传播范围）
		}
		
		// 创建组播处理器实例
		var err error
		ad.multicast, err = coap.NewMulticastHandler(multicastConfig)
		if err != nil {
			return fmt.Errorf("创建组播处理器失败: %w", err)
		}
		
		// 启动组播处理器（开始监听组播消息）
		if err := ad.multicast.Start(); err != nil {
			return fmt.Errorf("启动组播处理器失败: %w", err)
		}
	}

	// 更新服务运行状态
	ad.isRunning = true
	ad.currentRound = 0 // 重置当前轮次为0

	// 启动发现工作协程（发送发现消息）
	ad.wg.Add(1)
	go ad.discoveryWorker()

	// 启动消息接收协程（处理设备响应）
	ad.wg.Add(1)
	go ad.messageReceiver()

	// 打印启动日志
	ad.log.Info("主动发现服务启动成功",
		zap.String("本地设备ID", ad.localDevice.DeviceID),
		zap.Int("总发现轮次", ad.totalRounds))

	return nil
}

// Stop 停止主动设备发现服务，取消上下文、等待协程完成并释放组播资源
func (ad *ActiveDiscovery) Stop() {
	ad.mu.Lock()         // 写锁：保障停止过程的线程安全
	defer ad.mu.Unlock() // 函数退出时释放锁

	// 若服务未运行，直接返回
	if !ad.isRunning {
		return
	}

	ad.log.Info("开始停止主动发现服务")

	// 标记服务为停止状态，取消上下文（通知协程退出）
	ad.isRunning = false
	ad.cancel()
	
	// 等待所有核心协程（discoveryWorker/messageReceiver）完成
	ad.wg.Wait()

	// 停止组播处理器（离开组播组、关闭连接）
	if ad.multicast != nil {
		ad.multicast.Stop()
	}

	// 打印停止日志与统计信息
	ad.log.Info("主动发现服务已停止",
		zap.Uint64("发送消息总数", ad.messagesSent),
		zap.Uint64("完成轮次总数", ad.roundsCount))
}

// discoveryWorker 发现工作协程，循环执行发现轮次（发送消息+等待间隔）
// 协程退出条件：上下文取消或所有轮次完成
func (ad *ActiveDiscovery) discoveryWorker() {
	defer ad.wg.Done() // 协程退出时通知WaitGroup

	ad.log.Debug("发现工作协程启动")

	for {
		select {
		// 上下文取消（服务停止），退出协程
		case <-ad.ctx.Done():
			return
		// 执行单轮发现，若返回false（所有轮次完成）则退出
		default:
			if !ad.performDiscoveryRound() {
				return
			}
		}
	}
}

// performDiscoveryRound 执行单轮发现：发送一次发现消息，等待下一轮间隔，更新轮次状态
// 返回：true=继续下一轮，false=所有轮次完成或服务停止
func (ad *ActiveDiscovery) performDiscoveryRound() bool {
	// 读取当前轮次状态（使用读锁，避免阻塞写操作）
	ad.mu.RLock()
	// 若当前轮次≥总轮次，所有轮次完成，返回false
	if ad.currentRound >= ad.totalRounds {
		ad.mu.RUnlock()
		ad.log.Info("所有发现轮次已完成")
		return false
	}
	// 获取当前轮次的间隔与轮次编号（避免后续锁内操作耗时）
	interval := ad.intervals[ad.currentRound]
	currentRoundNum := ad.currentRound
	ad.mu.RUnlock()

	// 1. 发送发现消息
	if err := ad.sendDiscoveryMessage(); err != nil {
		ad.log.Error("发送发现消息失败",
			zap.Int("当前轮次", currentRoundNum),
			zap.Error(err))
		// 若注册了错误回调，触发回调（通知外部）
		if ad.onError != nil {
			ad.onError(err)
		}
	} else {
		ad.log.Debug("发现消息发送成功",
			zap.Int("当前轮次", currentRoundNum),
			zap.Duration("下一轮间隔", interval))
		// 更新统计信息：发送消息数+1（使用写锁）
		ad.mu.Lock()
		ad.messagesSent++
		ad.mu.Unlock()
	}

	// 2. 等待下一轮间隔（期间若服务停止则退出）
	select {
	case <-ad.ctx.Done(): // 服务停止，返回false
		return false
	case <-time.After(interval): // 间隔结束，更新轮次状态
		ad.mu.Lock()
		ad.currentRound++   // 当前轮次+1
		ad.roundsCount++    // 完成轮次+1
		ad.mu.Unlock()
		return true // 继续下一轮
	}
}

// sendDiscoveryMessage 发送发现消息的完整流程：构建业务消息→编码→构建CoAP消息→编码→组播发送
// 返回：若任一环节失败则返回错误
func (ad *ActiveDiscovery) sendDiscoveryMessage() error {
	// 1. 构建业务层发现消息（基于protocol模块，包含本地设备信息）
	protocolMsg := ad.buildDiscoveryMessage()
	
	// 2. 编码业务消息（如JSON序列化）
	protocolData, err := protocolMsg.Encode()
	if err != nil {
		return fmt.Errorf("编码业务消息失败: %w", err)
	}

	// 3. 构建CoAP协议层消息（设置消息类型、方法、选项等）
	coapMsg := ad.buildCoAPMessage(protocolData)
	
	// 4. 编码CoAP消息（转为网络传输字节流）
	coapData, err := ad.encoder.Encode(coapMsg)
	if err != nil {
		return fmt.Errorf("编码CoAP消息失败: %w", err)
	}

	// 5. 通过组播处理器发送消息（向所有支持组播的接口广播）
	if err := ad.multicast.SendMulticast(coapData); err != nil {
		return fmt.Errorf("发送组播消息失败: %w", err)
	}

	return nil
}

// buildDiscoveryMessage 构建业务层发现消息（基于protocol.Message）
// 功能：填充本地设备信息、业务数据、服务标识、设备能力等
// 返回：业务层发现消息实例
func (ad *ActiveDiscovery) buildDiscoveryMessage() *protocol.Message {
	ad.mu.RLock()         // 读锁：保障读取配置时的线程安全
	defer ad.mu.RUnlock() // 函数退出时释放锁

	// 创建业务层发现消息（传入本地设备信息）
	msg := protocol.NewDiscoverMessage(ad.localDevice)
	
	// 若配置了业务数据，添加到消息中
	if ad.settings != nil && ad.settings.BusinessData != "" {
		msg.BusinessData = ad.settings.BusinessData
	}
	
	// 添加服务标识（用于识别协议版本）
	msg.ServiceData = "nstackx-go/1.0"
	
	// 若本地设备类型非0，添加设备能力扩展信息
	if ad.localDevice.DeviceType != 0 {
		msg.Extra = map[string]interface{}{
			"deviceType": ad.localDevice.DeviceType, // 设备类型
			"version":    "1.0",                    // 协议版本
		}
	}

	return msg
}

// buildCoAPMessage 构建CoAP协议层消息（基于coap.RawMessage）
// 功能：设置消息类型（NON）、方法（GET）、消息ID、令牌、选项、payload
// 参数：payload - 业务层编码后的字节流（如JSON）
// 返回：CoAP协议消息实例
func (ad *ActiveDiscovery) buildCoAPMessage(payload []byte) *coap.RawMessage {
	// 生成随机消息ID（2字节，避免消息混淆，符合CoAP协议要求）
	messageID := uint16(rand.Uint32())
	
	// 1. 创建CoAP消息：非确认型（NON）、方法为GET（0x01=0.01）、消息ID为随机值
	msg := coap.CreateMessage(coap.TypeNonConfirmable, 0x01, messageID)
	
	// 2. 添加Uri-Path选项：资源路径为"/device/discover"（与设备发现端点匹配）
	msg.AddOption(coap.OptionUriPath, []byte("device"))
	msg.AddOption(coap.OptionUriPath, []byte("discover"))
	
	// 3. 添加Content-Format选项：声明payload格式为JSON（50=ContentFormatJSON）
	msg.AddOption(coap.OptionContentFormat, []byte{coap.ContentFormatJSON})
	
	// 4. 设置payload（业务层编码后的数据）
	msg.SetPayload(payload)
	
	// 5. 生成4字节随机令牌（Token）：用于跟踪消息（可选，增强上下文关联）
	token := make([]byte, 4)
	rand.Read(token)
	msg.SetToken(token)

	return msg
}

// messageReceiver 消息接收协程，从组播处理器的消息队列中读取响应并交给processReceivedMessage处理
// 协程退出条件：上下文取消或消息队列关闭
func (ad *ActiveDiscovery) messageReceiver() {
	defer ad.wg.Done() // 协程退出时通知WaitGroup

	// 若组播处理器未初始化，直接退出
	if ad.multicast == nil {
		return
	}

	// 获取组播消息接收通道（只读）
	msgChan := ad.multicast.GetMessageChannel()
	
	for {
		select {
		// 上下文取消（服务停止），退出协程
		case <-ad.ctx.Done():
			return
		// 从通道读取组播消息（若通道关闭则ok=false）
		case multicastMsg, ok := <-msgChan:
			if !ok {
				return
			}
			// 处理收到的组播消息
			ad.processReceivedMessage(multicastMsg)
		}
	}
}

// processReceivedMessage 处理收到的组播消息：解码CoAP→校验响应→解码业务消息→转换设备信息→触发回调
// 参数：multicastMsg - 组播处理器收到的消息（含数据、来源、接口等）
func (ad *ActiveDiscovery) processReceivedMessage(multicastMsg *coap.MulticastMessage) {
	// 过滤空消息（数据为空则跳过）
	if multicastMsg == nil || len(multicastMsg.Data) == 0 {
		return
	}

	// 1. 解码CoAP消息（从字节流转为coap.RawMessage）
	coapMsg, err := ad.encoder.Decode(multicastMsg.Data)
	if err != nil {
		ad.log.Debug("解码CoAP消息失败", zap.Error(err))
		return
	}

	// 2. 校验消息类型：仅处理2.05 Content响应（设备发现的成功响应）
	if coapMsg.Code != 0x45 { // 0x45=CoAP的2.05 Content状态码
		return
	}

	// 3. 过滤空payload（无设备信息则跳过）
	if len(coapMsg.Payload) == 0 {
		return
	}

	// 4. 解码业务层消息（从CoAP payload转为protocol.Message）
	protocolMsg, err := protocol.Decode(coapMsg.Payload)
	if err != nil {
		ad.log.Debug("解码业务消息失败", zap.Error(err))
		return
	}

	// 5. 校验业务消息合法性（如必填字段是否存在）
	if err := protocolMsg.Validate(); err != nil {
		ad.log.Debug("业务消息非法", zap.Error(err))
		return
	}

	// 6. 过滤非发现响应消息（仅处理响应或发现类型的消息）
	if protocolMsg.Type != protocol.MessageTypeResponse &&
		protocolMsg.Type != protocol.MessageTypeDiscover {
		return
	}

	// 7. 将业务消息转换为设备信息结构体（api.DeviceInfo）
	deviceInfo, err := protocolMsg.ToDeviceInfo()
	if err != nil {
		ad.log.Debug("转换为设备信息失败", zap.Error(err))
		return
	}

	// 8. 过滤自身设备（避免处理自己发送的响应）
	if deviceInfo.DeviceID == ad.localDevice.DeviceID {
		return
	}

	// 9. 补充设备网络信息（从组播消息中提取）
	if udpAddr, ok := multicastMsg.Source.(*net.UDPAddr); ok {
		deviceInfo.NetworkIP = udpAddr.IP // 设备IP地址
	}
	deviceInfo.NetworkName = multicastMsg.Interface.Name // 接收消息的网络接口名

	// 打印设备发现日志
	ad.log.Info("发现新设备",
		zap.String("设备ID", deviceInfo.DeviceID),
		zap.String("设备名称", deviceInfo.DeviceName),
		zap.String("设备IP", deviceInfo.NetworkIP.String()))

	// 10. 触发设备发现回调（若注册），通知外部新设备
	if ad.onDeviceFound != nil {
		ad.onDeviceFound(deviceInfo)
	}
}

// SetDiscoverySettings 更新发现配置，同时重新初始化间隔列表
// 参数：settings - 新的发现配置（如广播次数、间隔）
func (ad *ActiveDiscovery) SetDiscoverySettings(settings *api.DiscoverySettings) {
	ad.mu.Lock()         // 写锁：保障配置更新与间隔初始化的原子性
	defer ad.mu.Unlock() // 函数退出时释放锁

	// 更新配置并重新初始化间隔（覆盖原有间隔）
	ad.settings = settings
	ad.initializeIntervals()
}

// GetStatistics 获取发现服务的统计信息
// 返回：已发送消息数，已完成轮次总数
func (ad *ActiveDiscovery) GetStatistics() (uint64, uint64) {
	ad.mu.RLock()         // 读锁：保障统计信息读取的线程安全
	defer ad.mu.RUnlock() // 函数退出时释放锁

	return ad.messagesSent, ad.roundsCount
}

// IsRunning 查询主动发现服务是否正在运行
// 返回：true=运行中，false=已停止
func (ad *ActiveDiscovery) IsRunning() bool {
	ad.mu.RLock()         // 读锁：避免阻塞写操作
	defer ad.mu.RUnlock() // 函数退出时释放锁

	return ad.isRunning
}

// GetCurrentRound 获取当前发现轮次状态
// 返回：当前轮次（从0开始），总轮次
func (ad *ActiveDiscovery) GetCurrentRound() (int, int) {
	ad.mu.RLock()         // 读锁：保障轮次信息读取的线程安全
	defer ad.mu.RUnlock() // 函数退出时释放锁

	return ad.currentRound, ad.totalRounds
}
