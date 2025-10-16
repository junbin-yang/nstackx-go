// CoAP客户端实现，支持 CoAP 协议的单播（Unicast）、组播（Multicast）消息发送和设备发现请求的发送与响应处理
package coap

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/network"
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp"
	"go.uber.org/zap"
)

const (
	MulticastIPv4  = "224.0.1.187"   // IPv4组播地址（CoAP标准组播地址）
	MulticastIPv6  = "ff02::1"       // IPv6组播地址
	BroadcastPort  = 5683            // CoAP默认端口
	DefaultTimeout = 5 * time.Second // 默认超时时间
)

// Client 表示一个CoAP客户端实例
type Client struct {
	mu sync.RWMutex // 读写锁，用于保护并发访问的资源

	conn      *udp.ClientConn  // CoAP UDP连接实例
	multicast net.PacketConn   // 组播连接
	timeout   time.Duration    // 超时时间设置
	log       *logger.Logger   // 日志实例
	netmgr    *network.Manager // 网络管理器
}

// NewClient 创建一个新的CoAP客户端
func NewClient(netmgr *network.Manager) *Client {
	return &Client{
		timeout: DefaultTimeout,   // 设置默认超时时间
		log:     logger.Default(), // 绑定日志实例
		netmgr:  netmgr,           // 注入网络管理器
	}
}

// SendBroadcast 发送广播消息（实际通过组播实现）
// 发送设备发现广播：
// 用protocol包创建发现消息
// localDevice := &api.LocalDeviceInfo{/* 填充本地设备信息 */}
// discoverMsg := protocol.NewDiscoverMessage(localDevice)
// 将业务消息序列化为JSON字节流（业务层编码）
// payload, err := discoverMsg.Encode()
// 创建CoAP消息（coap.RawMessage），并将JSON字节流作为Payload
// coapMsg := coap.CreateMessage(
//    coap.TypeNonConfirmable, // 消息类型（如NON）
//    codes.GET,               // CoAP方法（如GET）
//    generateMessageID(),     // 消息ID
//)
// oapMsg.SetPayload(payload)               // 设置Payload为业务数据
// coapMsg.AddOption(coap.OptionUriPath, []byte(DeviceDiscoverPath)) // 添加CoAP选项（如资源路径）
// 通过CoAP编码器将消息编码为网络传输字节流（协议层编码）
// coapEncoder := coap.NewEncoder()
// data, err := coapEncoder.Encode(coapMsg)
// 发送
// c.SendBroadcast(data)
// func generateMessageID() uint16 {
//    // 取当前时间的纳秒级时间戳，对65536（2^16）取模，确保结果是2字节
//    return uint16(time.Now().UnixNano() % 65536)
// }
func (c *Client) SendBroadcast(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 使用网络管理器获取适合多播的接口
	if c.netmgr == nil {
		return fmt.Errorf("未注入网络管理器")
	}
	interfaces := c.netmgr.GetMulticastInterfaces()

	// 遍历每个网络接口发送消息
	for _, iface := range interfaces {
		// 按 IPv4 发送（同一接口发送一次即可）
		sent := false
		for _, ip := range iface.Addresses {
			if ip.To4() == nil || ip.IsLoopback() {
				continue
			}
			if err := c.sendMulticast(MulticastIPv4, BroadcastPort, data); err != nil {
				c.log.Warn("发送IPv4组播失败",
					zap.String("interface", iface.Name),
					zap.Error(err))
			} else {
				sent = true
			}
			if sent {
				break
			}
		}
	}

	return nil
}

// sendMulticast 发送组播消息（内部实现）
func (c *Client) sendMulticast(ip string, port int, data []byte) error {
	// 解析组播地址
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("解析地址失败: %w", err)
	}

	// 建立UDP连接
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("建立UDP连接失败: %w", err)
	}
	defer conn.Close()

	// 发送消息
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}

	c.log.Debug("已发送组播消息",
		zap.String("addr", addr.String()),
		zap.Int("size", len(data)))

	return nil
}

// SendUnicast 向特定设备发送单播消息
func (c *Client) SendUnicast(ip net.IP, port int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	addr := fmt.Sprintf("%s:%d", ip.String(), port)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// 建立UDP客户端连接
	conn, err := udp.Dial(addr)
	if err != nil {
		return fmt.Errorf("连接到%s失败: %w", addr, err)
	}
	defer conn.Close()

	// 创建CoAP消息
	msg := conn.NewMessage(ctx)
	msg.SetCode(codes.POST)               // 设置消息类型为POST
	msg.SetBody(message.ByteString(data)) // 设置消息体

	// 发送消息
	if err := conn.WriteMessage(msg); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}

	c.log.Debug("已发送单播消息",
		zap.String("addr", addr),
		zap.Int("size", len(data)))

	return nil
}

// SendMessage 向特定IP发送消息（使用默认CoAP端口）
func (c *Client) SendMessage(ip net.IP, data []byte) error {
	return c.SendUnicast(ip, BroadcastPort, data)
}

// SendDiscoveryRequest 发送设备发现请求并等待响应
func (c *Client) SendDiscoveryRequest(ip net.IP) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	addr := fmt.Sprintf("%s:%d", ip.String(), DefaultCoAPPort)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// 建立UDP客户端连接
	conn, err := udp.Dial(addr)
	if err != nil {
		return fmt.Errorf("连接到%s失败: %w", addr, err)
	}
	defer conn.Close()

	// 创建CoAP消息
	msg := conn.NewMessage(ctx)
	msg.SetCode(codes.GET) // 设置消息类型为GET

	// 发送消息并等待响应
	resp, err := conn.Do(msg)
	if err != nil {
		return fmt.Errorf("发送发现请求失败: %w", err)
	}

	// 处理响应
	if resp != nil {
		body, _ := resp.ReadBody()
		c.log.Debug("收到发现响应",
			zap.String("from", addr),
			zap.Int("size", len(body)))
	}

	return nil
}

// buildDiscoveryMessage 构建CoAP发现消息
func (c *Client) buildDiscoveryMessage(msg *protocol.Message) ([]byte, error) {
	// 1. 验证并编码payload
	if err := msg.Validate(); err != nil {
		return nil, fmt.Errorf("消息不合法: %w", err)
	}
	payload, err := msg.Encode()
	if err != nil {
		return nil, fmt.Errorf("编码消息失败: %w", err)
	}

	// 2. 构建CoAP头部（4字节）
	// 版本(2位)=1 + 类型(2位)=NON（非确认消息） + Token长度(4位)=0
	header := []byte{0x50}
	// 消息码：根据消息类型设置（如discover用GET，response用2.05 Content）
	switch msg.Type {
	case protocol.MessageTypeDiscover:
		header = append(header, byte(codes.GET)) // GET: 0.01
	case protocol.MessageTypeResponse:
		header = append(header, byte(codes.Content)) // 2.05 Content
	default:
		header = append(header, byte(codes.POST)) // 默认POST
	}
	// 消息ID（2字节，随机生成避免冲突）
	msgID := uint16(time.Now().UnixNano() % 65536)
	header = append(header, byte(msgID>>8), byte(msgID&0xFF))

	// 3. 构建CoAP选项（如Uri-Path，资源路径）
	// 假设communication_dsoftbus期望的发现路径为"/discovery"
	options := c.encodeOption(11, []byte("/discovery")) // 11是Uri-Path的标准选项号

	// 4. 拼接头部、选项、payload（带分隔符）
	var coapMsg []byte
	coapMsg = append(coapMsg, header...)
	coapMsg = append(coapMsg, options...)
	if len(payload) > 0 {
		coapMsg = append(coapMsg, 0xFF) // payload分隔符
		coapMsg = append(coapMsg, payload...)
	}

	return coapMsg, nil
}

// encodeOption 编码CoAP选项
func (c *Client) encodeOption(optionNumber int, value []byte) []byte {
	var result []byte
	// 基础选项号（0-14）可直接用4位delta表示
	delta := optionNumber
	if delta > 14 {
		// 超过14需用扩展字段（简化处理，实际需按标准分阶段编码）
		result = append(result, 0x0F) // delta=15表示需要扩展
		result = append(result, byte(delta))
	} else {
		result = append(result, byte(delta<<4)) // 高4位存delta
	}

	// 长度编码（0-14直接用4位，超过需扩展）
	length := len(value)
	if length > 14 {
		result[len(result)-1] |= 0x0F // 低4位=15表示需要扩展长度
		result = append(result, byte(length))
	} else {
		result[len(result)-1] |= byte(length) // 低4位存长度
	}

	// 追加选项值
	result = append(result, value...)
	return result
}

// SetTimeout 设置客户端超时时间
func (c *Client) SetTimeout(timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.timeout = timeout
}

// Close 关闭客户端所有连接
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	if c.multicast != nil {
		c.multicast.Close()
		c.multicast = nil
	}

	return nil
}
