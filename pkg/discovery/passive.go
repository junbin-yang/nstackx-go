// 实现被动设备发现功能，核心逻辑为“接收外部发现请求后响应”，而非主动发送请求
// 特点：通过延迟响应、速率限制避免网络风暴，同时能解析其他设备的响应以发现设备
package discovery

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/discovery/coap"     // 依赖CoAP协议模块（组播、编解码）
	"github.com/junbin-yang/nstackx-go/pkg/discovery/protocol" // 依赖业务层协议模块（设备消息结构）
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

const (
	// 响应延迟范围：用于避免多设备同时响应导致的“响应风暴”（网络拥堵）
	MinResponseDelay = 20 * time.Millisecond  // 最小响应延迟
	MaxResponseDelay = 500 * time.Millisecond // 最大响应延迟

	// 速率限制：控制响应频率，防止频繁响应占用过多网络资源
	MaxResponsesPerSecond = 10                     // 每秒最大响应次数
	ResponseCooldown      = 100 * time.Millisecond // 两次响应的最小间隔（冷却时间）
)

// PassiveDiscovery 被动设备发现核心结构体，管理请求监听、响应发送、限流与统计
type PassiveDiscovery struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问时的资源安全

	// Configuration 配置相关字段（初始化时传入，核心为本地设备信息）
	localDevice *api.LocalDeviceInfo   // 本地设备信息（用于构建响应消息，标识自身）
	settings    *api.DiscoverySettings // 发现配置（可选，如自定义业务数据）

	// Network components 网络通信组件
	multicast *coap.MulticastHandler // CoAP组播处理器（接收外部发现请求、发送响应）
	encoder   *coap.Encoder          // CoAP消息编解码器（协议层消息处理）

	// Discovery state 发现服务运行状态
	isRunning bool // 服务运行标记（true=运行中，false=已停止）

	// Rate limiting 速率限制相关字段（防止频繁响应）
	lastResponseTime time.Time // 上次发送响应的时间（用于冷却时间判断）
	responseCount    int       // 当前时间窗口内的响应次数（用于速率限制）
	responseWindow   time.Time // 速率限制的时间窗口（每1秒重置一次）

	// Control 流程控制组件
	ctx    context.Context    // 上下文，用于控制协程退出
	cancel context.CancelFunc // 上下文取消函数，触发服务停止
	wg     sync.WaitGroup     // 等待组，确保停止时所有协程完成

	// Callbacks 外部回调函数（通知外部请求/设备发现/错误事件）
	onDeviceFound func(*api.DeviceInfo) // 设备发现回调（解析到其他设备时触发）
	onRequest     func(source net.Addr) // 请求接收回调（收到外部发现请求时触发）
	onError       func(error)           // 错误回调（响应发送失败等错误时触发）

	// Statistics 服务运行统计信息
	requestsReceived uint64 // 接收的发现请求总数
	responsesSent    uint64 // 发送的响应总数
	requestsIgnored  uint64 // 因限流被忽略的请求总数

	log *zap.Logger // 日志实例（基于zap框架，打印请求/响应日志）
}

// PassiveDiscoveryConfig 被动发现的初始化配置结构体，聚合所需外部依赖与回调
type PassiveDiscoveryConfig struct {
	LocalDevice   *api.LocalDeviceInfo   // 本地设备信息（必填，用于构建响应）
	Settings      *api.DiscoverySettings // 发现配置（可选，如自定义业务数据）
	Multicast     *coap.MulticastHandler // CoAP组播处理器（可选，外部传入或内部创建）
	OnDeviceFound func(*api.DeviceInfo)  // 设备发现回调（可选，通知外部新设备）
	OnRequest     func(source net.Addr)  // 请求接收回调（可选，通知外部收到请求）
	OnError       func(error)            // 错误回调（可选，通知外部错误）
}

// NewPassiveDiscovery 创建被动设备发现实例，初始化配置、上下文与核心组件
// 参数：config - 初始化配置（LocalDevice必填，其他可选）
// 返回：发现实例，若配置无效（如LocalDevice为nil）则返回错误
func NewPassiveDiscovery(config *PassiveDiscoveryConfig) (*PassiveDiscovery, error) {
	// 校验核心配置：config与LocalDevice不能为nil
	if config == nil || config.LocalDevice == nil {
		return nil, fmt.Errorf("无效配置（config或LocalDevice不能为空）")
	}

	// 初始化开发环境日志（忽略初始化错误，简化处理）
	logger, _ := zap.NewDevelopment()
	// 创建可取消上下文，用于控制服务生命周期
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化被动发现核心字段
	pd := &PassiveDiscovery{
		localDevice:      config.LocalDevice,
		settings:         config.Settings,
		multicast:        config.Multicast,
		encoder:          coap.NewEncoder(), // 创建CoAP编解码器实例
		onDeviceFound:    config.OnDeviceFound,
		onRequest:        config.OnRequest,
		onError:          config.OnError,
		ctx:              ctx,
		cancel:           cancel,
		lastResponseTime: time.Now().Add(-ResponseCooldown), // 初始冷却时间已过
		responseWindow:   time.Now(),                        // 初始时间窗口为当前时间
		log:              logger,
	}

	return pd, nil
}

// Start 启动被动设备发现服务，初始化组播处理器（若未提供）并启动请求监听协程
// 返回：若服务已运行或组播处理器创建失败则返回错误
func (pd *PassiveDiscovery) Start() error {
	pd.mu.Lock()         // 写锁：保障启动过程的线程安全
	defer pd.mu.Unlock() // 函数退出时释放锁

	// 若服务已运行，直接返回错误
	if pd.isRunning {
		return fmt.Errorf("被动发现服务已运行")
	}

	// 若未提供外部组播处理器，创建默认组播配置并初始化
	if pd.multicast == nil {
		multicastConfig := &coap.MulticastConfig{
			IPv4Enabled: true,            // 启用IPv4组播（接收IPv4发现请求）
			IPv6Enabled: true,            // 启用IPv6组播（接收IPv6发现请求）
			Port:        coap.CoAPPort,   // 使用CoAP默认端口5683
			TTL:         coap.DefaultTTL, // 默认TTL=255（最大传播范围）
		}

		// 创建组播处理器实例
		var err error
		pd.multicast, err = coap.NewMulticastHandler(multicastConfig)
		if err != nil {
			return fmt.Errorf("创建组播处理器失败: %w", err)
		}

		// 启动组播处理器（开始监听组播消息）
		if err := pd.multicast.Start(); err != nil {
			return fmt.Errorf("启动组播处理器失败: %w", err)
		}
	}

	// 标记服务为运行状态
	pd.isRunning = true

	// 启动请求监听协程（接收外部发现请求）
	pd.wg.Add(1)
	go pd.requestListener()

	// 打印启动日志
	pd.log.Info("被动发现服务启动成功",
		zap.String("本地设备ID", pd.localDevice.DeviceID))

	return nil
}

// Stop 停止被动设备发现服务，取消上下文、等待协程完成并释放组播资源
func (pd *PassiveDiscovery) Stop() {
	pd.mu.Lock()         // 写锁：保障停止过程的线程安全
	defer pd.mu.Unlock() // 函数退出时释放锁

	// 若服务未运行，直接返回
	if !pd.isRunning {
		return
	}

	pd.log.Info("开始停止被动发现服务")

	// 标记服务为停止状态，取消上下文（通知协程退出）
	pd.isRunning = false
	pd.cancel()

	// 等待所有核心协程（requestListener/sendDelayedResponse）完成
	pd.wg.Wait()

	// 停止组播处理器（离开组播组、关闭连接）
	if pd.multicast != nil {
		pd.multicast.Stop()
	}

	// 打印停止日志与统计信息
	pd.log.Info("被动发现服务已停止",
		zap.Uint64("接收请求总数", pd.requestsReceived),
		zap.Uint64("发送响应总数", pd.responsesSent),
		zap.Uint64("忽略请求总数", pd.requestsIgnored))
}

// requestListener 请求监听协程，从组播处理器的消息队列中读取请求并交给processReceivedMessage处理
// 协程退出条件：上下文取消或消息队列关闭
func (pd *PassiveDiscovery) requestListener() {
	defer pd.wg.Done() // 协程退出时通知WaitGroup

	// 若组播处理器未初始化，直接退出
	if pd.multicast == nil {
		return
	}

	// 获取组播消息接收通道（只读）
	msgChan := pd.multicast.GetMessageChannel()

	pd.log.Debug("请求监听协程启动")

	for {
		select {
		// 上下文取消（服务停止），退出协程
		case <-pd.ctx.Done():
			return
		// 从通道读取组播消息（若通道关闭则ok=false）
		case multicastMsg, ok := <-msgChan:
			if !ok {
				return
			}
			// 处理收到的组播消息（区分请求/响应）
			pd.processReceivedMessage(multicastMsg)
		}
	}
}

// processReceivedMessage 处理收到的组播消息：解码CoAP→区分请求/响应→分别处理
// 参数：multicastMsg - 组播处理器收到的消息（含数据、来源IP、接收接口等）
func (pd *PassiveDiscovery) processReceivedMessage(multicastMsg *coap.MulticastMessage) {
	// 过滤空消息（数据为空则跳过）
	if multicastMsg == nil || len(multicastMsg.Data) == 0 {
		return
	}

	// 1. 解码CoAP消息（从字节流转为coap.RawMessage）
	coapMsg, err := pd.encoder.Decode(multicastMsg.Data)
	if err != nil {
		pd.log.Debug("解码CoAP消息失败", zap.Error(err))
		return
	}

	// 2. 区分消息类型：是发现请求（GET）还是其他设备的响应（2.05）
	if coapMsg.Code == 0x01 { // 0x01=CoAP的GET方法（发现请求）
		// 检查是否为目标发现端点（路径匹配）
		if !pd.isDiscoveryRequest(coapMsg) {
			return
		}

		// 更新统计：接收请求数+1
		pd.mu.Lock()
		pd.requestsReceived++
		pd.mu.Unlock()

		pd.log.Debug("收到发现请求",
			zap.String("来源地址", multicastMsg.Source.String()),
			zap.String("接收接口", multicastMsg.Interface.Name))

		// 触发请求接收回调（通知外部收到请求）
		if pd.onRequest != nil {
			pd.onRequest(multicastMsg.Source)
		}

		// 3. 速率限制判断：是否允许发送响应
		if !pd.shouldRespond() {
			// 更新统计：忽略请求数+1
			pd.mu.Lock()
			pd.requestsIgnored++
			pd.mu.Unlock()

			pd.log.Debug("触发速率限制，忽略当前请求")
			return
		}

		// 4. 计算随机延迟（避免多设备同时响应导致网络风暴）
		delay := pd.calculateResponseDelay()

		// 启动协程延迟发送响应（非阻塞，避免阻塞当前消息处理）
		pd.wg.Add(1)
		go pd.sendDelayedResponse(multicastMsg.Source, multicastMsg.Interface, coapMsg, delay)
	} else {
		// 非GET消息：可能是其他设备的响应，解析以发现设备
		pd.processDiscoveryResponse(coapMsg, multicastMsg)
	}
}

// isDiscoveryRequest 判断CoAP消息是否为目标发现请求（路径匹配）
// 匹配规则：支持两个标准发现路径：/device/discover 和 /.well-known/core
// 参数：coapMsg - 解码后的CoAP消息
// 返回：true=是发现请求，false=不是
func (pd *PassiveDiscovery) isDiscoveryRequest(coapMsg *coap.RawMessage) bool {
	// 从CoAP消息中获取Uri-Path选项（资源路径段列表）
	pathSegments := coapMsg.GetOptions(coap.OptionUriPath)

	// 规则1：匹配路径 "/device/discover"（自定义设备发现端点）
	if len(pathSegments) >= 2 {
		if string(pathSegments[0]) == "device" && string(pathSegments[1]) == "discover" {
			return true
		}
	}

	// 规则2：匹配路径 "/.well-known/core"（CoAP标准资源发现端点）
	if len(pathSegments) >= 2 {
		if string(pathSegments[0]) == ".well-known" && string(pathSegments[1]) == "core" {
			return true
		}
	}

	return false
}

// shouldRespond 基于速率限制判断是否允许发送响应（避免频繁响应）
// 限流规则：1. 两次响应间隔≥100ms（冷却时间）；2. 每秒响应≤10次
// 返回：true=允许响应，false=拒绝响应
func (pd *PassiveDiscovery) shouldRespond() bool {
	pd.mu.Lock()         // 写锁：保障限流字段读写的原子性
	defer pd.mu.Unlock() // 函数退出时释放锁

	now := time.Now()

	// 规则1：检查冷却时间（上次响应后需等待≥100ms）
	if now.Sub(pd.lastResponseTime) < ResponseCooldown {
		return false
	}

	// 规则2：时间窗口重置（每1秒重置一次响应计数）
	if now.Sub(pd.responseWindow) > time.Second {
		pd.responseWindow = now
		pd.responseCount = 0
	}

	// 规则3：检查每秒最大响应次数（≤10次）
	if pd.responseCount >= MaxResponsesPerSecond {
		return false
	}

	return true
}

// calculateResponseDelay 计算随机响应延迟（避免多设备同时响应导致网络风暴）
// 延迟范围：MinResponseDelay（20ms）~ MaxResponseDelay（500ms）
// 返回：随机延迟时长
func (pd *PassiveDiscovery) calculateResponseDelay() time.Duration {
	// 计算延迟范围（最大-最小）
	delayRange := MaxResponseDelay - MinResponseDelay
	// 生成范围内的随机延迟
	randomDelay := time.Duration(rand.Int63n(int64(delayRange)))
	// 返回最终延迟（基础最小延迟+随机偏移）
	return MinResponseDelay + randomDelay
}

// sendDelayedResponse 延迟发送响应：等待指定时间后，构建并发送CoAP响应
// 参数：target - 响应目标地址；iface - 接收请求的网络接口；request - 原始请求消息；delay - 延迟时长
func (pd *PassiveDiscovery) sendDelayedResponse(target net.Addr, iface net.Interface, request *coap.RawMessage, delay time.Duration) {
	defer pd.wg.Done() // 协程退出时通知WaitGroup

	// 1. 等待延迟时间（期间若服务停止则退出）
	select {
	case <-pd.ctx.Done(): // 服务停止，取消响应
		return
	case <-time.After(delay): // 延迟结束，开始发送响应
	}

	// 2. 构建CoAP响应消息（基于原始请求，确保令牌、消息ID匹配）
	responseMsg := pd.buildDiscoveryResponse(request)

	// 3. 发送响应到目标地址
	if err := pd.sendResponse(target, iface, responseMsg); err != nil {
		pd.log.Error("发送发现响应失败",
			zap.String("目标地址", target.String()),
			zap.Error(err))
		// 触发错误回调（通知外部发送失败）
		if pd.onError != nil {
			pd.onError(err)
		}
	} else {
		// 更新统计信息：响应数+1，上次响应时间=当前时间，当前窗口响应数+1
		pd.mu.Lock()
		pd.responsesSent++
		pd.lastResponseTime = time.Now()
		pd.responseCount++
		pd.mu.Unlock()

		pd.log.Debug("发现响应发送成功",
			zap.String("目标地址", target.String()),
			zap.Duration("延迟时间", delay))
	}
}

// buildDiscoveryResponse 构建CoAP响应消息（协议层），确保与原始请求匹配
// 核心逻辑：复用请求的令牌和消息ID，设置响应状态码（2.05）、Content-Format和Payload
// 参数：request - 原始发现请求消息
// 返回：CoAP响应消息实例
func (pd *PassiveDiscovery) buildDiscoveryResponse(request *coap.RawMessage) *coap.RawMessage {
	// 1. 创建CoAP响应：非确认型（NON）、状态码2.05 Content（成功响应）、复用请求的消息ID
	response := coap.CreateMessage(coap.TypeNonConfirmable, 0x45, request.MessageID)

	// 2. 复用请求的令牌（Token）：确保请求与响应关联
	if len(request.Token) > 0 {
		response.SetToken(request.Token)
	}

	// 3. 构建业务层响应消息（protocol.Message，含本地设备信息）
	protocolMsg := pd.buildProtocolResponse()

	// 4. 编码业务层消息（如JSON序列化）
	payloadData, err := protocolMsg.Encode()
	if err != nil {
		pd.log.Error("编码业务层响应消息失败", zap.Error(err))
		return response // 编码失败仍返回基础响应（无有效Payload）
	}

	// 5. 添加Content-Format选项：声明Payload格式为JSON（50=ContentFormatJSON）
	response.AddOption(coap.OptionContentFormat, []byte{coap.ContentFormatJSON})

	// 6. 设置响应Payload（编码后的业务数据）
	response.SetPayload(payloadData)

	return response
}

// buildProtocolResponse 构建业务层响应消息（protocol.Message）
// 功能：填充本地设备信息、业务数据、服务标识、额外元数据（如时间戳、模式）
// 返回：业务层响应消息实例
func (pd *PassiveDiscovery) buildProtocolResponse() *protocol.Message {
	pd.mu.RLock()         // 读锁：保障读取配置时的线程安全
	defer pd.mu.RUnlock() // 函数退出时释放锁

	// 创建业务层响应消息（传入本地设备信息，空业务数据默认）
	msg := protocol.NewResponseMessage(pd.localDevice, "")

	// 若配置了自定义业务数据，覆盖默认空值
	if pd.settings != nil && pd.settings.BusinessData != "" {
		msg.BusinessData = pd.settings.BusinessData
	}

	// 添加服务标识（用于识别协议版本）
	msg.ServiceData = "nstackx-go/1.0"

	// 添加额外元数据（模式、版本、时间戳）
	msg.Extra = map[string]interface{}{
		"mode":      "passive",         // 发现模式（被动）
		"version":   "1.0",             // 协议版本
		"timestamp": time.Now().Unix(), // 响应生成时间戳（秒级）
	}

	return msg
}

// sendResponse 发送CoAP响应到目标地址（UDP单播，非组播）
// 参数：target - 目标地址；iface - 发送使用的网络接口；msg - 待发送的CoAP响应
// 返回：若拨号或发送失败则返回错误
func (pd *PassiveDiscovery) sendResponse(target net.Addr, iface net.Interface, msg *coap.RawMessage) error {
	// 1. 编码CoAP响应（转为网络传输字节流）
	coapData, err := pd.encoder.Encode(msg)
	if err != nil {
		return fmt.Errorf("编码CoAP响应失败: %w", err)
	}

	// 2. 转换目标地址为UDP地址（确保是UDP协议）
	udpTarget, ok := target.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("无效的目标地址类型（需为UDP地址）")
	}

	// 3. 建立UDP连接（拨号到目标地址）
	conn, err := net.DialUDP("udp", nil, udpTarget)
	if err != nil {
		return fmt.Errorf("UDP拨号失败: %w", err)
	}
	defer conn.Close() // 确保函数退出时关闭连接

	// 4. 发送响应数据
	_, err = conn.Write(coapData)
	if err != nil {
		return fmt.Errorf("发送响应失败: %w", err)
	}

	return nil
}

// processDiscoveryResponse 处理其他设备的发现响应，解析出设备信息并触发回调
// 功能：过滤自身设备、补充网络信息、触发设备发现回调
// 参数：coapMsg - 解码后的CoAP响应；multicastMsg - 原始组播消息（含来源IP、接口）
func (pd *PassiveDiscovery) processDiscoveryResponse(coapMsg *coap.RawMessage, multicastMsg *coap.MulticastMessage) {
	// 1. 仅处理2.05 Content响应（设备发现的成功响应）
	if coapMsg.Code != 0x45 {
		return
	}

	// 2. 过滤空Payload（无设备信息则跳过）
	if len(coapMsg.Payload) == 0 {
		return
	}

	// 3. 解码业务层消息（从CoAP Payload转为protocol.Message）
	protocolMsg, err := protocol.Decode(coapMsg.Payload)
	if err != nil {
		pd.log.Debug("解码业务层响应消息失败", zap.Error(err))
		return
	}

	// 4. 校验业务消息合法性（如必填字段是否存在）
	if err := protocolMsg.Validate(); err != nil {
		pd.log.Debug("业务消息非法", zap.Error(err))
		return
	}

	// 5. 过滤非响应类型消息（仅处理响应或发现类型）
	if protocolMsg.Type != protocol.MessageTypeResponse &&
		protocolMsg.Type != protocol.MessageTypeDiscover {
		return
	}

	// 6. 将业务消息转换为设备信息结构体（v1.DeviceInfo）
	deviceInfo, err := protocolMsg.ToDeviceInfo()
	if err != nil {
		pd.log.Debug("转换为设备信息失败", zap.Error(err))
		return
	}

	// 7. 过滤自身设备（避免处理自己发送的响应）
	if deviceInfo.DeviceID == pd.localDevice.DeviceID {
		return
	}

	// 8. 补充设备网络信息（从组播消息中提取）
	if udpAddr, ok := multicastMsg.Source.(*net.UDPAddr); ok {
		deviceInfo.NetworkIP = udpAddr.IP // 设备IP地址
	}
	deviceInfo.NetworkName = multicastMsg.Interface.Name // 接收消息的网络接口名

	// 打印设备发现日志
	pd.log.Info("被动模式下发现新设备",
		zap.String("设备ID", deviceInfo.DeviceID),
		zap.String("设备名称", deviceInfo.DeviceName),
		zap.String("设备IP", deviceInfo.NetworkIP.String()))

	// 9. 触发设备发现回调（通知外部新设备）
	if pd.onDeviceFound != nil {
		pd.onDeviceFound(deviceInfo)
	}
}

// SendDirectResponse 向指定IP发送直接响应（跳过请求监听，主动发送响应）
// 场景：需主动向某个设备回复，而非等待其请求
// 参数：targetIP - 目标设备IP；businessData - 自定义业务数据
// 返回：若服务未运行或发送失败则返回错误
func (pd *PassiveDiscovery) SendDirectResponse(targetIP net.IP, businessData string) error {
	pd.mu.Lock()         // 写锁：保障服务状态判断与统计更新的原子性
	defer pd.mu.Unlock() // 函数退出时释放锁

	// 若服务未运行，返回错误
	if !pd.isRunning {
		return fmt.Errorf("被动发现服务未运行")
	}

	// 1. 构建业务层响应消息（含自定义业务数据）
	protocolMsg := protocol.NewResponseMessage(pd.localDevice, businessData)

	// 2. 编码业务层消息（JSON序列化）
	payloadData, err := protocolMsg.Encode()
	if err != nil {
		return fmt.Errorf("编码业务层消息失败: %w", err)
	}

	// 3. 构建CoAP响应消息：非确认型、2.05状态码、随机消息ID
	coapMsg := coap.CreateMessage(coap.TypeNonConfirmable, 0x45, uint16(rand.Uint32()))
	// 添加Content-Format选项（JSON）
	coapMsg.AddOption(coap.OptionContentFormat, []byte{coap.ContentFormatJSON})
	// 设置Payload（编码后的业务数据）
	coapMsg.SetPayload(payloadData)

	// 4. 编码CoAP消息（转为字节流）
	coapData, err := pd.encoder.Encode(coapMsg)
	if err != nil {
		return fmt.Errorf("编码CoAP消息失败: %w", err)
	}

	// 5. 构建目标UDP地址（CoAP默认端口5683）
	targetAddr := &net.UDPAddr{
		IP:   targetIP,
		Port: coap.CoAPPort,
	}

	// 6. 建立UDP连接并发送
	conn, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		return fmt.Errorf("UDP拨号失败: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write(coapData)
	if err != nil {
		return fmt.Errorf("发送直接响应失败: %w", err)
	}

	// 更新统计：响应数+1
	pd.responsesSent++
	pd.log.Info("直接响应发送成功",
		zap.String("目标IP", targetIP.String()),
		zap.Int("业务数据长度", len(businessData)))

	return nil
}

// GetStatistics 获取被动发现服务的运行统计信息
// 返回：接收请求总数，发送响应总数，被忽略请求总数
func (pd *PassiveDiscovery) GetStatistics() (uint64, uint64, uint64) {
	pd.mu.RLock()         // 读锁：保障统计信息读取的线程安全
	defer pd.mu.RUnlock() // 函数退出时释放锁

	return pd.requestsReceived, pd.responsesSent, pd.requestsIgnored
}

// IsRunning 查询被动发现服务是否正在运行
// 返回：true=运行中，false=已停止
func (pd *PassiveDiscovery) IsRunning() bool {
	pd.mu.RLock()         // 读锁：避免阻塞写操作
	defer pd.mu.RUnlock() // 函数退出时释放锁

	return pd.isRunning
}
