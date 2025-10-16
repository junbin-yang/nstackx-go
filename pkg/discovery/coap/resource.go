// 提供CoAP资源管理功能，含资源注册、请求处理、观察者机制与资源发现
package coap

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// ResourceHandler 资源请求处理函数类型，用于定义具体资源的请求处理逻辑
// 参数：request - 接收的CoAP请求
// 返回：处理后生成的CoAP响应
type ResourceHandler func(request *Request) *Response

// Request 表示一个完整的CoAP请求，封装请求元数据与消息内容
type Request struct {
	Message   *RawMessage       // 原始CoAP消息（含头部、选项、负载）
	Source    net.Addr          // 请求来源地址（发送方的IP:Port）
	Path      string            // 请求的资源路径（从Uri-Path选项提取，如"/device/discover"）
	Query     map[string]string // 请求的查询参数（从Uri-Query选项提取，如"?id=123"）
	Timestamp time.Time         // 请求接收时间戳
}

// Response 表示一个完整的CoAP响应，封装响应状态与数据
type Response struct {
	Code        uint8    // 响应状态码（如0x45=2.05 Content表示成功，0x84=4.04 Not Found表示资源不存在）
	ContentType uint16   // 响应负载的内容格式（对应ContentFormat常量，如ContentFormatJSON=50）
	Payload     []byte   // 响应负载数据（业务数据，如JSON字符串）
	Options     []Option // 响应的CoAP选项（可选，如Location-Path）
}

// Resource 表示一个CoAP资源，对应CoAP协议中的“可访问资源”（类似HTTP的URL资源）
type Resource struct {
	Path         string            // 资源路径（唯一标识，如"/device/info"）
	Title        string            // 资源标题（描述性信息，如"Device Information"）
	ResourceType string            // 资源类型（RT属性，用于分类，如"nstackx.info"）
	Interface    string            // 资源接口（IF属性，描述访问方式，如"nstackx.i"）
	Handler      ResourceHandler   // 资源的请求处理器（关联具体业务逻辑）
	Observable   bool              // 是否支持观察者机制（OBserve选项，true=支持订阅通知）
	Attributes   map[string]string // 资源扩展属性（如"obs"表示可观察，"title"表示标题）
}

// ResourceManager CoAP资源管理器，负责资源的生命周期管理、请求分发与观察者维护
type ResourceManager struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问资源/观察者列表的安全性

	resources map[string]*Resource  // 已注册的资源列表（key=资源路径，value=资源实例）
	observers map[string][]Observer // 资源的观察者列表（key=资源路径，value=观察者数组）
	discovery *DiscoveryResource    // 资源发现处理器（负责/.well-known/core端点）

	log *logger.Logger // 日志实例（基于zap框架，用于打印资源管理日志）
}

// Observer 表示一个资源观察者，对应CoAP的OBserve机制（客户端订阅资源变更通知）
type Observer struct {
	Address    net.Addr  // 观察者地址（客户端的IP:Port）
	Token      []byte    // 观察者的令牌（Token，用于关联请求与通知）
	Path       string    // 订阅的资源路径（如"/notification"）
	LastNotify time.Time // 上次发送通知的时间戳（用于控制通知频率）
}

// DiscoveryResource 资源发现处理器，专门处理`/.well-known/core`端点的请求
// 该端点是CoAP标准的资源发现地址（RFC 6690），返回所有已注册资源的列表
type DiscoveryResource struct {
	manager *ResourceManager // 关联的资源管理器，用于获取已注册资源
}

// NewResourceManager 创建CoAP资源管理器实例，初始化核心结构并注册默认资源
func NewResourceManager() *ResourceManager {
	// 初始化资源管理器核心字段
	rm := &ResourceManager{
		resources: make(map[string]*Resource),  // 初始化资源映射
		observers: make(map[string][]Observer), // 初始化观察者映射
		log:       logger.Default(),
	}

	// 初始化资源发现处理器（关联当前管理器）
	rm.discovery = &DiscoveryResource{manager: rm}

	// 注册默认资源（无需外部手动注册，启动即有）
	rm.registerDefaultResources()

	return rm
}

// registerDefaultResources 注册CoAP默认资源，包含标准发现端点与常用业务端点
func (rm *ResourceManager) registerDefaultResources() {
	// 1. 注册标准资源发现端点：/.well-known/core（RFC 6690规定，用于枚举所有资源）
	rm.RegisterResource(&Resource{
		Path:         "/.well-known/core",
		Title:        "Resource Discovery", // 资源标题：资源发现
		ResourceType: "core.rd",            // 资源类型：核心发现资源
		Interface:    "core.ll",            // 资源接口：核心轻量级接口
		Handler:      rm.discovery.Handle,  // 处理器：调用DiscoveryResource的Handle方法
		Observable:   false,                // 不支持观察者（资源列表变更频率低，无需订阅）
	})

	// 2. 注册设备发现端点：/device/discover（自定义业务端点，用于设备发现）
	rm.RegisterResource(&Resource{
		Path:         "/device/discover",
		Title:        "Device Discovery",      // 资源标题：设备发现
		ResourceType: "nstackx.discover",      // 资源类型：nstackx框架设备发现
		Interface:    "nstackx.d",             // 资源接口：nstackx设备接口
		Handler:      rm.handleDeviceDiscover, // 处理器：默认设备发现逻辑
		Observable:   true,                    // 支持观察者（设备列表变更时通知订阅者）
	})

	// 3. 注册设备信息端点：/device/info（自定义业务端点，用于获取设备信息）
	rm.RegisterResource(&Resource{
		Path:         "/device/info",
		Title:        "Device Information", // 资源标题：设备信息
		ResourceType: "nstackx.info",       // 资源类型：nstackx框架设备信息
		Interface:    "nstackx.i",          // 资源接口：nstackx信息接口
		Handler:      rm.handleDeviceInfo,  // 处理器：默认设备信息逻辑
		Observable:   false,                // 不支持观察者（设备信息变更频率低）
	})

	// 4. 注册通知端点：/notification（自定义业务端点，用于推送通知）
	rm.RegisterResource(&Resource{
		Path:         "/notification",
		Title:        "Notifications",       // 资源标题：通知
		ResourceType: "nstackx.notify",      // 资源类型：nstackx框架通知
		Interface:    "nstackx.n",           // 资源接口：nstackx通知接口
		Handler:      rm.handleNotification, // 处理器：默认通知逻辑
		Observable:   true,                  // 支持观察者（有新通知时主动推送）
	})
}

// RegisterResource 注册一个新的CoAP资源（线程安全）
// 参数：resource - 待注册的资源实例
// 返回：注册失败返回错误（如资源为nil、路径为空或已存在）
func (rm *ResourceManager) RegisterResource(resource *Resource) error {
	// 校验资源合法性：资源不能为nil，路径不能为空
	if resource == nil || resource.Path == "" {
		return fmt.Errorf("无效的资源（资源为nil或路径为空）")
	}

	rm.mu.Lock()         // 写锁：保障资源注册时的线程安全
	defer rm.mu.Unlock() // 函数退出时释放锁

	// 规范化资源路径：确保路径以"/"开头（统一格式，如"device/info"→"/device/info"）
	if !strings.HasPrefix(resource.Path, "/") {
		resource.Path = "/" + resource.Path
	}

	// 校验资源是否已注册（路径唯一，避免覆盖）
	if _, exists := rm.resources[resource.Path]; exists {
		return fmt.Errorf("资源已注册：%s", resource.Path)
	}

	// 初始化资源属性映射（若为nil）
	if resource.Attributes == nil {
		resource.Attributes = make(map[string]string)
	}

	// 添加资源标准属性（自动填充到Attributes，用于/.well-known/core返回）
	if resource.Title != "" {
		resource.Attributes["title"] = resource.Title // 标题属性
	}
	if resource.ResourceType != "" {
		resource.Attributes["rt"] = resource.ResourceType // 资源类型属性（RT）
	}
	if resource.Interface != "" {
		resource.Attributes["if"] = resource.Interface // 接口属性（IF）
	}
	if resource.Observable {
		resource.Attributes["obs"] = "" // 可观察属性（空值表示存在该属性）
	}

	// 注册资源到映射表
	rm.resources[resource.Path] = resource

	rm.log.Info("资源注册成功",
		zap.String("路径", resource.Path),
		zap.String("标题", resource.Title))

	return nil
}

// UnregisterResource 注销一个CoAP资源（线程安全）
// 参数：path - 待注销的资源路径
// 返回：true=注销成功（资源存在），false=注销失败（资源不存在）
func (rm *ResourceManager) UnregisterResource(path string) bool {
	rm.mu.Lock()         // 写锁：保障资源注销时的线程安全
	defer rm.mu.Unlock() // 函数退出时释放锁

	// 检查资源是否存在
	if _, exists := rm.resources[path]; !exists {
		return false
	}

	// 从资源映射中删除资源
	delete(rm.resources, path)
	// 同时删除该资源的所有观察者（资源不存在，观察者无意义）
	delete(rm.observers, path)

	rm.log.Info("资源注销成功", zap.String("路径", path))
	return true
}

// GetResource 根据路径获取已注册的CoAP资源（线程安全）
// 参数：path - 资源路径
// 返回：资源实例，资源是否存在的布尔值
func (rm *ResourceManager) GetResource(path string) (*Resource, bool) {
	rm.mu.RLock()         // 读锁：仅读取资源，不修改
	defer rm.mu.RUnlock() // 函数退出时释放锁

	resource, exists := rm.resources[path]
	return resource, exists
}

// ListResources 获取所有已注册的CoAP资源（返回副本，避免外部修改）
// 返回：资源列表副本
func (rm *ResourceManager) ListResources() []*Resource {
	rm.mu.RLock()         // 读锁：保障读取时资源列表不被修改
	defer rm.mu.RUnlock() // 函数退出时释放锁

	// 初始化资源列表（容量=资源数量，避免扩容）
	resources := make([]*Resource, 0, len(rm.resources))
	// 遍历资源映射，添加到列表
	for _, resource := range rm.resources {
		resources = append(resources, resource)
	}

	return resources
}

// HandleRequest 处理CoAP请求，是资源管理器的核心请求入口（线程安全）
// 参数：request - 待处理的CoAP请求
// 返回：处理生成的CoAP响应（含错误响应）
func (rm *ResourceManager) HandleRequest(request *Request) *Response {
	// 校验请求合法性：请求或原始消息不能为nil
	if request == nil || request.Message == nil {
		return rm.createErrorResponse(0x80) // 返回4.00 Bad Request（请求无效）
	}

	// 1. 从CoAP消息的Uri-Path选项提取资源路径（如选项["device","discover"]→"/device/discover"）
	path := rm.extractPath(request.Message)
	request.Path = path

	// 2. 从CoAP消息的Uri-Query选项提取查询参数（如选项["id=123","name=test"]→map{"id":"123","name":"test"}）
	request.Query = rm.extractQuery(request.Message)

	rm.log.Debug("开始处理CoAP请求",
		zap.String("资源路径", path),
		zap.Uint8("请求类型", request.Message.Code))

	// 3. 根据路径查找对应的资源
	resource, exists := rm.GetResource(path)
	if !exists {
		return rm.createErrorResponse(0x84) // 返回4.04 Not Found（资源不存在）
	}

	// 4. 校验资源是否有请求处理器（无处理器则无法处理请求）
	if resource.Handler == nil {
		return rm.createErrorResponse(0x85) // 返回4.05 Method Not Allowed（方法不允许）
	}

	// 5. 若资源支持观察者机制，处理观察者注册/注销（OBserve选项）
	if resource.Observable {
		rm.handleObserve(request, resource)
	}

	// 6. 调用资源的请求处理器，执行具体业务逻辑
	response := resource.Handler(request)
	// 若处理器返回nil（业务逻辑异常），返回500错误
	if response == nil {
		response = rm.createErrorResponse(0xA0) // 返回5.00 Internal Server Error（服务器内部错误）
	}

	return response
}

// extractPath 从CoAP消息的Uri-Path选项中提取资源路径
// 参数：msg - 原始CoAP消息
// 返回：规范化的资源路径（如"/"或"/device/discover"）
func (rm *ResourceManager) extractPath(msg *RawMessage) string {
	// 从消息中获取所有Uri-Path选项（可能有多个段，如["device","discover"]）
	pathSegments := msg.GetOptions(OptionUriPath)
	// 无Uri-Path选项时，默认路径为"/"（根路径）
	if len(pathSegments) == 0 {
		return "/"
	}

	// 将路径段转换为字符串（如[]byte("device")→"device"）
	parts := make([]string, len(pathSegments))
	for i, segment := range pathSegments {
		parts[i] = string(segment)
	}

	// 拼接路径段为完整路径（如["device","discover"]→"/device/discover"）
	path := "/" + strings.Join(parts, "/")
	return path
}

// extractQuery 从CoAP消息的Uri-Query选项中提取查询参数（键值对）
// 参数：msg - 原始CoAP消息
// 返回：查询参数映射（如map{"id":"123","name":""}）
func (rm *ResourceManager) extractQuery(msg *RawMessage) map[string]string {
	// 从消息中获取所有Uri-Query选项（如[]byte("id=123")、[]byte("name")）
	queryOptions := msg.GetOptions(OptionUriQuery)
	// 无Uri-Query选项时，返回nil
	if len(queryOptions) == 0 {
		return nil
	}

	// 解析每个查询选项为键值对
	query := make(map[string]string)
	for _, option := range queryOptions {
		param := string(option) // 转换为字符串（如"id=123"）
		// 按"="分割（最多分割1次，处理"key=value=xxx"场景，保留value中的"="）
		parts := strings.SplitN(param, "=", 2)
		if len(parts) == 2 {
			// 有值的参数（如"id=123"→key="id", value="123"）
			query[parts[0]] = parts[1]
		} else {
			// 无值的参数（如"name"→key="name", value=""）
			query[parts[0]] = ""
		}
	}

	return query
}

// handleObserve 处理CoAP的OBserve选项（6号选项），实现观察者注册/注销
// 参数：request - CoAP请求（含OBserve选项）；resource - 目标资源（需支持Observable）
func (rm *ResourceManager) handleObserve(request *Request, resource *Resource) {
	// 获取OBserve选项值（选项号6，CoAP标准定义）
	observeValue, hasObserve := request.Message.GetOption(6)
	// 无OBserve选项，直接返回
	if !hasObserve {
		return
	}

	rm.mu.Lock()         // 写锁：保障观察者列表修改的线程安全
	defer rm.mu.Unlock() // 函数退出时释放锁

	// OBserve值为0或空→注册观察者（客户端订阅资源变更）
	if len(observeValue) == 0 || observeValue[0] == 0 {
		// 构建观察者实例
		observer := Observer{
			Address:    request.Source,        // 观察者地址（客户端IP:Port）
			Token:      request.Message.Token, // 观察者令牌（关联后续通知）
			Path:       resource.Path,         // 订阅的资源路径
			LastNotify: time.Now(),            // 初始通知时间（当前时间）
		}

		// 若该资源的观察者列表未初始化，先初始化
		if rm.observers[resource.Path] == nil {
			rm.observers[resource.Path] = make([]Observer, 0)
		}

		// 将观察者添加到列表
		rm.observers[resource.Path] = append(rm.observers[resource.Path], observer)

		rm.log.Info("观察者注册成功",
			zap.String("资源路径", resource.Path),
			zap.String("观察者地址", request.Source.String()))
	} else {
		// OBserve值为1→注销观察者（客户端取消订阅）
		rm.removeObserver(resource.Path, request.Source)
	}
}

// removeObserver 从指定资源的观察者列表中删除某个观察者（内部调用，需先加锁）
// 参数：path - 资源路径；address - 观察者地址
func (rm *ResourceManager) removeObserver(path string, address net.Addr) {
	// 获取该资源的观察者列表
	observers := rm.observers[path]
	if observers == nil {
		return // 无观察者，直接返回
	}

	// 筛选保留非目标地址的观察者（删除目标观察者）
	newObservers := make([]Observer, 0)
	for _, obs := range observers {
		if obs.Address.String() != address.String() {
			newObservers = append(newObservers, obs)
		}
	}

	// 更新观察者列表（覆盖原列表）
	rm.observers[path] = newObservers

	rm.log.Info("观察者注销成功",
		zap.String("资源路径", path),
		zap.String("观察者地址", address.String()))
}

// NotifyObservers 向指定资源的所有观察者发送通知（资源变更时调用）
// 参数：path - 资源路径；payload - 通知的负载数据（资源变更内容）
func (rm *ResourceManager) NotifyObservers(path string, payload []byte) {
	rm.mu.RLock()                   // 读锁：仅读取观察者列表，不修改
	observers := rm.observers[path] // 获取该资源的所有观察者
	rm.mu.RUnlock()                 // 提前释放读锁（避免通知过程阻塞其他读操作）

	// 无观察者，直接返回
	if len(observers) == 0 {
		return
	}

	rm.log.Info("开始通知资源观察者",
		zap.String("资源路径", path),
		zap.Int("观察者数量", len(observers)))

	// 遍历所有观察者，逐个发送通知
	for _, observer := range observers {
		// 1. 创建通知消息：非确认型（NON）、状态码2.05 Content（成功）
		msg := CreateMessage(TypeNonConfirmable, 0x45, 0) // 消息ID暂填0（实际需动态生成）
		msg.SetToken(observer.Token)                      // 设置观察者令牌（关联订阅请求）
		msg.SetPayload(payload)                           // 设置通知负载（资源变更数据）

		// TODO: 发送通知到观察者（待实现：通过UDP发送CoAP消息到observer.Address）
		_ = msg // 占位，避免未使用变量报错
	}
}

// createErrorResponse 创建CoAP错误响应（统一错误响应生成逻辑）
// 参数：code - 错误状态码（如0x80=4.00，0x84=4.04）
// 返回：错误响应实例
func (rm *ResourceManager) createErrorResponse(code uint8) *Response {
	return &Response{
		Code: code, // 错误状态码（无负载，仅返回状态）
	}
}

// ------------------------------ 默认资源处理器 ------------------------------
// handleDeviceDiscover 设备发现资源的默认处理器（处理/device/discover请求）
func (rm *ResourceManager) handleDeviceDiscover(request *Request) *Response {
	// 注：实际项目中需替换为真实业务逻辑（如返回在线设备列表）
	// 此处为示例：返回JSON格式的成功响应
	return &Response{
		Code:        0x45,                      // 2.05 Content（请求成功）
		ContentType: ContentFormatJSON,         // 内容格式：JSON（50）
		Payload:     []byte(`{"status":"ok"}`), // 负载：成功状态
	}
}

// handleDeviceInfo 设备信息资源的默认处理器（处理/device/info请求）
func (rm *ResourceManager) handleDeviceInfo(request *Request) *Response {
	// 注：实际项目中需替换为真实业务逻辑（如返回设备型号、版本等）
	// 此处为示例：返回JSON格式的设备标识
	return &Response{
		Code:        0x45,                              // 2.05 Content（请求成功）
		ContentType: ContentFormatJSON,                 // 内容格式：JSON（50）
		Payload:     []byte(`{"device":"nstackx-go"}`), // 负载：设备标识
	}
}

// handleNotification 通知资源的默认处理器（处理/notification请求）
func (rm *ResourceManager) handleNotification(request *Request) *Response {
	// 注：实际项目中需替换为真实业务逻辑（如接收客户端通知订阅或发送通知）
	// 此处为示例：返回2.04 Changed（请求已处理且资源已变更）
	return &Response{
		Code: 0x44, // 2.04 Changed（请求处理成功，资源状态变更）
	}
}

// ------------------------------ DiscoveryResource 实现 ------------------------------
// Handle 处理/.well-known/core端点的请求，返回CoRE链接格式的资源列表（RFC 6690）
func (dr *DiscoveryResource) Handle(request *Request) *Response {
	// 从资源管理器获取所有已注册资源
	resources := dr.manager.ListResources()

	// 构建CoRE链接格式的响应内容（每个资源对应一个链接字符串）
	links := make([]string, 0, len(resources))

	// 遍历资源，格式化每个资源为CoRE链接格式
	for _, resource := range resources {
		link := dr.formatResourceLink(resource)
		links = append(links, link)
	}

	// 拼接所有链接（用逗号分隔，符合CoRE链接格式规范）
	payload := strings.Join(links, ",")

	// 返回响应：2.05成功，内容格式为CoRE链接格式（40）
	return &Response{
		Code:        0x45,                    // 2.05 Content（请求成功）
		ContentType: ContentFormatLinkFormat, // 内容格式：CoRE链接格式（40）
		Payload:     []byte(payload),         // 负载：链接列表
	}
}

// formatResourceLink 将资源格式化为CoRE链接格式字符串（RFC 6690）
// 格式示例：</device/info>;title="Device Information";rt="nstackx.info";if="nstackx.i"
func (dr *DiscoveryResource) formatResourceLink(resource *Resource) string {
	// 链接主体：资源路径用尖括号包裹（如</device/info>）
	link := fmt.Sprintf("<%s>", resource.Path)

	// 添加资源属性（如title、rt、if等，从resource.Attributes获取）
	for key, value := range resource.Attributes {
		if value == "" {
			// 空值属性（如obs）：格式为";obs"
			link += fmt.Sprintf(";%s", key)
		} else {
			// 有值属性（如title）：格式为`;title="Device Information"`
			link += fmt.Sprintf(`;%s="%s"`, key, value)
		}
	}

	return link
}

// ResourceDiscovery （待实现）远程主机的CoAP资源发现
// 功能：向远程主机的/.well-known/core发送GET请求，解析CoRE链接格式响应，返回资源列表
// 参数：host - 远程主机IP；port - 远程主机CoAP端口
// 返回：远程主机的资源列表，错误信息
func ResourceDiscovery(host string, port int) ([]*Resource, error) {
	// TODO: 实现CoAP资源发现逻辑（步骤：1. 创建GET请求；2. 发送到host:port；3. 解析响应）
	return nil, nil
}
