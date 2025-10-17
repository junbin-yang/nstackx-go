// 提供用于设备发现的CoAP协议实现，基于go-coap库封装服务器功能
package coap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	//coapNet "github.com/plgd-dev/go-coap/v3/net"
	//"github.com/plgd-dev/go-coap/v3/options"
	//"github.com/plgd-dev/go-coap/v3/udp"
	//"github.com/plgd-dev/go-coap/v3/udp/server"
	"go.uber.org/zap"
)

const (
	DiscoveryPath      = "/.well-known/core" // CoAP标准资源发现路径（RFC 6690）
	DeviceDiscoverPath = "/device/discover"  // 自定义设备发现路径
	DeviceResponsePath = "/device/response"  // 自定义设备响应路径（设备回复发现请求）
	NotificationPath   = "/notification"     // 自定义通知路径（设备状态变更通知）
)

// Message 封装CoAP消息的核心信息，简化外部对CoAP消息的处理
type Message struct {
	Code       codes.Code // CoAP消息码（如GET、POST、2.05 Content等）
	Token      []byte     // 消息令牌（用于关联请求与响应）
	Payload    []byte     // 消息负载（业务数据，如设备信息JSON）
	SourceIP   net.IP     // 消息来源IP地址
	SourcePort int        // 消息来源端口
	Path       string     // 消息对应的资源路径（如"/device/discover"）
}

// MessageHandler 消息处理回调函数类型，用于外部自定义消息处理逻辑
// 参数：*Message - 解析后的CoAP消息
type MessageHandler func(*Message)

// Server CoAP服务器实例，管理服务器状态、路由和请求处理
type Server struct {
	mu sync.RWMutex // 读写锁，保障多协程并发访问服务器资源的安全性

	multicast *MulticastHandler  // 多播处理器（用于发送广播消息）
	handler   MessageHandler     // 外部设置的消息处理回调函数
	ctx       context.Context    // 服务器上下文，用于控制生命周期
	cancel    context.CancelFunc // 上下文取消函数，用于停止服务器
	log       *logger.Logger     // 日志实例（基于zap框架）
}

// NewServer 创建CoAP服务器实例
// 参数：handler - 消息处理回调函数（外部业务逻辑入口）
// 返回：服务器实例，若handler为nil则返回错误
func NewServer(handler MessageHandler, multicast *MulticastHandler) (*Server, error) {
	if handler == nil {
		return nil, fmt.Errorf("消息处理回调函数不能为nil")
	}
	if multicast == nil {
		return nil, fmt.Errorf("多播处理器不能为nil")
	}

	// 创建可取消的上下文，用于控制服务器生命周期
	ctx, cancel := context.WithCancel(context.Background())

	// 初始化服务器核心字段
	s := &Server{
		multicast: multicast,
		handler:   handler,
		ctx:       ctx,
		cancel:    cancel,
		log:       logger.Default(),
	}

	// 配置预设路由（路径与处理函数的映射）
	s.setupRoutes()

	return s, nil
}

// setupRoutes 配置CoAP服务器的路由映射，关联路径与对应的请求处理函数
func (s *Server) setupRoutes() {
	router := s.multicast.GetRouter()

	// 资源发现端点：处理对/.well-known/core的请求（标准CoAP资源发现）
	router.Handle(DiscoveryPath, mux.HandlerFunc(s.handleDiscovery))

	// 设备发现端点：处理对/device/discover的请求（自定义设备发现）
	router.Handle(DeviceDiscoverPath, mux.HandlerFunc(s.handleDeviceDiscover))

	// 设备响应端点：处理对/device/response的请求（设备回复发现请求）
	router.Handle(DeviceResponsePath, mux.HandlerFunc(s.handleDeviceResponse))

	// 通知端点：处理对/notification的请求（设备状态变更通知）
	router.Handle(NotificationPath, mux.HandlerFunc(s.handleNotification))
}

// Start 启动CoAP服务器，开始监听指定端口的CoAP请求
// 返回：启动失败则返回错误（如端口被占用、服务器已启动等）
func (s *Server) Start() {
	s.mu.Lock()         // 写锁：保障启动过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁
	s.multicast.Start() // 启动多播处理器
}

// Stop 停止CoAP服务器，释放资源
func (s *Server) Stop() {
	s.mu.Lock()         // 写锁：保障停止过程的线程安全
	defer s.mu.Unlock() // 函数退出时释放锁

	// 触发上下文取消，通知所有依赖上下文的组件退出
	s.cancel()
	s.multicast.Stop()

	s.log.Info("CoAP服务器已停止")
}

// handleDiscovery 处理资源发现请求（路径：/.well-known/core）
// 功能：解析请求，调用外部回调，返回资源列表响应
func (s *Server) handleDiscovery(w mux.ResponseWriter, r *mux.Message) {
	s.log.Debug("收到资源发现请求",
		zap.String("来源", w.Conn().RemoteAddr().String()))

	// 解析请求为自定义Message结构
	msg := s.parseMessage(w, r, DiscoveryPath)

	// 调用外部消息处理回调（传递解析后的消息）
	if s.handler != nil {
		s.handler(msg)
	}

	// 构建响应：状态码2.05 Content（成功），内容格式为文本，负载为资源列表
	response := w.Conn().AcquireMessage(r.Context())
	defer w.Conn().ReleaseMessage(response)
	response.SetCode(codes.Content)
	response.SetToken(r.Token())
	response.SetContentFormat(message.TextPlain)
	// 设置响应体（调用buildDiscoveryResponse生成资源列表）
	response.SetBody(bytes.NewReader(s.buildDiscoveryResponse()))
	err := w.Conn().WriteMessage(response)
	if err != nil {
		s.log.Error("设置响应数据错误", zap.Error(err))
	}
}

// handleDeviceDiscover 处理设备发现请求（路径：/device/discover）
// 功能：解析请求，调用外部回调，返回确认响应
func (s *Server) handleDeviceDiscover(w mux.ResponseWriter, r *mux.Message) {
	s.log.Debug("收到设备发现请求",
		zap.String("来源", w.Conn().RemoteAddr().String()))

	// 解析请求为自定义Message结构
	msg := s.parseMessage(w, r, DeviceDiscoverPath)

	// 调用外部消息处理回调
	if s.handler != nil {
		s.handler(msg)
	}

	// 发送确认响应：状态码2.03 Valid（请求有效，已接收）
	w.SetResponse(codes.Valid, message.TextPlain, nil)
}

// handleDeviceResponse 处理设备响应消息（路径：/device/response）
// 功能：解析设备回复的发现响应，调用外部回调，返回确认
func (s *Server) handleDeviceResponse(w mux.ResponseWriter, r *mux.Message) {
	s.log.Debug("收到设备响应消息",
		zap.String("来源", w.Conn().RemoteAddr().String()))

	// 解析请求为自定义Message结构
	msg := s.parseMessage(w, r, DeviceResponsePath)

	// 调用外部消息处理回调
	if s.handler != nil {
		s.handler(msg)
	}

	// 发送确认响应：状态码2.03 Valid
	w.SetResponse(codes.Valid, message.TextPlain, nil)
}

// handleNotification 处理通知消息（路径：/notification）
// 功能：解析设备状态变更通知，调用外部回调，返回确认
func (s *Server) handleNotification(w mux.ResponseWriter, r *mux.Message) {
	s.log.Debug("收到通知消息",
		zap.String("来源", w.Conn().RemoteAddr().String()))

	// 解析请求为自定义Message结构
	msg := s.parseMessage(w, r, NotificationPath)

	// 调用外部消息处理回调
	if s.handler != nil {
		s.handler(msg)
	}

	// 发送确认响应：状态码2.03 Valid
	w.SetResponse(codes.Valid, message.TextPlain, nil)
}

// parseMessage 将go-coap库的mux.Message转换为自定义Message结构
// 功能：提取消息关键信息（码、令牌、负载、来源地址等），简化外部处理
func (s *Server) parseMessage(w mux.ResponseWriter, r *mux.Message, path string) *Message {
	// 读取消息负载（业务数据）
	payload, _ := r.ReadBody()

	// 获取消息来源地址（转换为UDP地址，提取IP和端口）
	addr := w.Conn().RemoteAddr().(*net.UDPAddr)

	// 构建并返回自定义Message
	return &Message{
		Code:       r.Code(),  // CoAP消息码
		Token:      r.Token(), // 消息令牌
		Payload:    payload,   // 消息负载
		SourceIP:   addr.IP,   // 来源IP
		SourcePort: addr.Port, // 来源端口
		Path:       path,      // 关联的资源路径
	}
}

// buildDiscoveryResponse 构建资源发现响应内容（CoRE Link Format格式）
// 功能：返回服务器支持的资源列表（示例实现，实际需根据注册的资源动态生成）
func (s *Server) buildDiscoveryResponse() []byte {
	// TODO: 实际应用中应根据服务器注册的资源动态生成响应
	// 示例：返回设备资源的链接格式（</device>;rt="nstackx.device"表示路径/device，类型为nstackx.device）
	return []byte(`</device>;rt="nstackx.device"`)
}
