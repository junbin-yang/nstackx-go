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
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/message/pool"
	"github.com/plgd-dev/go-coap/v3/udp"
	"github.com/plgd-dev/go-coap/v3/udp/client"
	"go.uber.org/zap"
)

const (
	DefaultCoAPPort = 5683            // 默认CoAP端口
	DefaultTimeout  = 5 * time.Second // 默认超时时间
)

// Client 表示一个CoAP客户端实例
type Client struct {
	mu sync.RWMutex // 读写锁，用于保护并发访问的资源

	conn      *client.Conn      // CoAP UDP连接实例
	multicast net.PacketConn    // 组播连接
	timeout   time.Duration     // 超时时间设置
	log       *logger.Logger    // 日志实例
	netmgr    *network.Manager  // 网络管理器
	mcast     *MulticastHandler // 组播处理器（统一广播入口）
}

// NewClient 创建一个新的CoAP客户端
func NewClient(netmgr *network.Manager, mh *MulticastHandler) *Client {
	return &Client{
		timeout: DefaultTimeout,   // 设置默认超时时间
		log:     logger.Default(), // 绑定日志实例
		netmgr:  netmgr,           // 注入网络管理器
		mcast:   mh,               // 注入组播处理器
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
	c.mu.RLock()
	mh := c.mcast
	c.mu.RUnlock()
	if mh == nil {
		return fmt.Errorf("未设置组播处理器（MulticastHandler），无法发送广播")
	}
	return mh.SendMulticast(data)
}

// SendUnicast 向特定设备发送单播原始字节（委派 MulticastHandler；端口固定为 CoAP 默认端口）
func (c *Client) SendUnicast(ip net.IP, data []byte) error {
	c.mu.RLock()
	mh := c.mcast
	c.mu.RUnlock()
	if mh == nil {
		return fmt.Errorf("未设置组播处理器（MulticastHandler），无法发送单播")
	}
	if ip == nil {
		return fmt.Errorf("无效的目标地址")
	}
	// 由 MulticastHandler 发送
	if ip.To4() != nil {
		return mh.SendUnicastIPv4(ip, data)
	}
	// IPv6 单播需提供 zone，请使用 MulticastHandler.SendUnicastIPv6(remoteIP, zone, payload)
	return fmt.Errorf("IPv6 单播请调用 SendUnicastIPv6(remoteIP, zone, payload)")
}

// SendDiscoveryRequest 发送设备发现请求并等待响应（未使用？）
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
	msg := pool.NewMessage(ctx)
	msg.SetCode(codes.GET) // 设置消息类型为GET
	msg.SetPath(DeviceDiscoverPath)

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

	// 不负责关闭 mcast（由拥有者Service/Handler自行管理）
	return nil
}
