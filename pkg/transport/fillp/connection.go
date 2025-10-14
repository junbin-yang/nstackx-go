// 实现FILLP (Fast Intelligent Lossless Link Protocol)协议，用于可靠的数据传输
package fillp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	
	"github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"go.uber.org/zap"
)

// 连接状态常量
const (
	StateIdle = iota      // 空闲状态：初始状态
	StateConnecting       // 连接中：正在建立连接
	StateConnected        // 已连接：连接已建立
	StateClosing          // 关闭中：正在关闭连接
	StateClosed           // 已关闭：连接已关闭
	StateListening 	      // 监听中：服务端正在等待客户端连接
)

// 协议常量定义
const (
	DefaultMTU          = 1400                  // 默认MTU(最大传输单元)大小
	DefaultWindowSize   = 65536                 // 默认窗口大小
	DefaultTimeout      = 30 * time.Second      // 默认连接超时时间
	DefaultKeepAlive    = 10 * time.Second      // 默认保活包发送间隔
	MaxRetransmissions  = 5                     // 最大重传次数
	InitialRTO          = 200 * time.Millisecond // 初始重传超时时间（降低以加快首次重传）
	MinRTO              = 50 * time.Millisecond  // 最小重传超时时间（本机回环可更激进）
	MaxRTO              = 10 * time.Second      // 最大重传超时时间
)

// 数据包类型常量
const (
	PacketTypeData = iota         // 数据报文
	PacketTypeAck                 // 确认报文
	PacketTypeSyn                 // 同步报文(用于建立连接)
	PacketTypeFin                 // 结束报文(用于关闭连接)
	PacketTypeKeepAlive           // 保活报文
	PacketTypeWindowUpdate        // 窗口更新报文
)

// Connection 表示一个FILLP连接
type Connection struct {
	mu sync.RWMutex // 读写锁，保护连接状态和属性

	// 连接信息
	localAddr  net.Addr        // 本地地址
	remoteAddr net.Addr        // 远程地址
	conn       net.PacketConn  // 底层数据包连接(UDP)
	state      int32           // 连接状态(原子操作)

	// 流量控制参数
	sendWindow    uint32 // 发送窗口大小
	receiveWindow uint32 // 接收窗口大小
	congestionWnd uint32 // 拥塞窗口大小
	ssthresh      uint32 // 慢启动阈值

	// 序列号
	sendSeq    uint32 // 发送序列号
	sendAck    uint32 // 发送确认号
	receiveSeq uint32 // 接收序列号
	receiveAck uint32 // 接收确认号

	// 快速重传
	lastAck     uint32 // 最近一次处理的ACK值
	dupAckCount int    // 重复ACK计数（=3时触发快速重传）

	// 缓冲区
	sendBuffer    *RingBuffer        // 发送缓冲区
	receiveBuffer *RingBuffer        // 接收缓冲区
	retransQueue  *RetransmissionQueue // 重传队列

	// 计时器相关
	rto          time.Duration // 重传超时时间
	srtt         time.Duration // 平滑RTT(往返时间)
	rttvar       time.Duration // RTT方差
	lastActivity time.Time     // 最后活动时间
	keepAliveTick *time.Ticker  // 保活计时器

	// 通道
	sendReady  chan struct{}  // 待发送数据通知通道
	recvReady  chan struct{}  // 接收数据就绪通知通道
	ackChan    chan uint32    // 确认接收通道
	closeChan  chan struct{}  // 关闭通知通道
	listenChan chan struct{}  // 服务端监听成功的通知通道

	// 上下文
	ctx    context.Context    // 上下文
	cancel context.CancelFunc // 取消函数

	// 统计信息
	stats ConnectionStats

	log *logger.Logger // 日志记录器
}

// ConnectionStats 包含连接的统计信息
type ConnectionStats struct {
	BytesSent          uint64    // 发送字节数
	BytesReceived      uint64    // 接收字节数
	PacketsSent        uint64    // 发送数据包数
	PacketsReceived    uint64    // 接收数据包数
	Retransmissions    uint64    // 重传次数
	DuplicateAcks      uint64    // 重复确认次数
	WindowUpdates      uint64    // 窗口更新次数
	RTT                time.Duration // 往返时间
	Bandwidth          uint64    // 带宽(字节/秒)
	PacketLoss         float64   // 丢包率
}

// Packet 表示一个FILLP数据包
type Packet struct {
	Type      uint8  // 包类型
	Flags     uint8  // 标志位
	Sequence  uint32 // 序列号
	Ack       uint32 // 确认号
	Window    uint32 // 窗口大小
	Timestamp uint32 // 时间戳
	Data      []byte // 数据内容
	Checksum  uint32 // 校验和
}

// 创建一个新的FILLP连接
// localAddr: 本地地址（服务端必填，客户端可选）
// remoteAddr: 远程地址（客户端必填，服务端可选）
func NewConnection(localAddr, remoteAddr net.Addr) (*Connection, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// 基本参数校验
	if localAddr != nil {
		// 验证本地地址是否为UDP类型
		if _, ok := localAddr.(*net.UDPAddr); !ok {
			return nil, fmt.Errorf("localAddr must be a UDP address")
		}
	}
	if remoteAddr != nil {
		// 验证远程地址是否为UDP类型
		if _, ok := remoteAddr.(*net.UDPAddr); !ok {
			return nil, fmt.Errorf("remoteAddr must be a UDP address")
		}
	}

	conn := &Connection{
		localAddr:     localAddr,
		remoteAddr:    remoteAddr,
		state:         StateIdle,
		sendWindow:    DefaultWindowSize,
		receiveWindow: DefaultWindowSize,
		congestionWnd: DefaultMTU * 2, // 初始拥塞窗口为2个MSS
		ssthresh:      DefaultWindowSize,
		sendBuffer:    NewRingBuffer(DefaultWindowSize),
		receiveBuffer: NewRingBuffer(DefaultWindowSize),
		retransQueue:  NewRetransmissionQueue(),
		rto:           InitialRTO,
		sendReady:     make(chan struct{}, 1),
		recvReady:     make(chan struct{}, 1),
		ackChan:       make(chan uint32, 100),
		closeChan:     make(chan struct{}),
		listenChan:    make(chan struct{}, 1),
		ctx:           ctx,
		cancel:        cancel,
		log:           logger.Default(),
	}

	return conn, nil
}

// 建立一个FILLP连接
func (c *Connection) Connect() error {
	c.mu.Lock()

	// 状态校验
	if c.state != StateIdle {
		return fmt.Errorf("connection not in idle state (current: %d)", c.state)
	}

	if c.remoteAddr == nil {
		return fmt.Errorf("remote address not set (use NewConnection with remoteAddr)")
	}

	// 绑定本地地址（客户端可选，为空则随机端口）
	bindAddr := ":0"
	if c.localAddr != nil {
		localUDPAddr, ok := c.localAddr.(*net.UDPAddr)
		if !ok {
			return fmt.Errorf("localAddr is not a UDP address")
		}
		bindAddr = localUDPAddr.String()
	}

	// 创建UDP连接
	conn, err := net.ListenPacket("udp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to create UDP socket on %s: %w", bindAddr, err)
	}

	// 初始化连接属性
	c.conn = conn
	c.localAddr = conn.LocalAddr() // 更新为实际绑定的本地地址（可能与传入的不同，如端口随机时）
	atomic.StoreInt32(&c.state, StateConnecting)

	c.mu.Unlock()

	// 启动工作协程
	go c.receiveWorker()
	go c.sendWorker()
	go c.retransmissionWorker()
	go c.keepAliveWorker()

	// 发送SYN包（初始序列号）
	c.sendSeq = 1000 // 实际场景应使用随机值
	if err := c.sendSyn(); err != nil {
		conn.Close()
		atomic.StoreInt32(&c.state, StateIdle)
		return fmt.Errorf("failed to send SYN: %w", err)
	}

	// 等待服务端SYN-ACK
	timeout := time.NewTimer(DefaultTimeout)
	defer timeout.Stop()

	select {
	case ackSeq := <-c.ackChan:
		if ackSeq != c.sendSeq+1 {
			c.Close()
			return fmt.Errorf("invalid SYN-ACK (expected ack %d, got %d)", c.sendSeq+1, ackSeq)
		}
		// 连接建立
		atomic.StoreInt32(&c.state, StateConnected)
		c.receiveAck = ackSeq
		c.sendSeq++
		c.log.Info("Client connected",
			logger.String("local", c.localAddr.String()),
			logger.String("remote", c.remoteAddr.String()))
		return nil
	case <-timeout.C:
		c.Close()
		return fmt.Errorf("connection timeout (remote: %s)", c.remoteAddr)
	case <-c.ctx.Done():
		return fmt.Errorf("connection cancelled")
	}
}

// 服务端监听方法：绑定本地地址并等待客户端连接
func (c *Connection) Listen() error {
        c.mu.Lock()

	// 状态校验
	if c.state != StateIdle {
		return fmt.Errorf("connection not in idle state (current: %d)", c.state)
	}

	// 本地地址校验（服务端必须在 NewConnection 中设置 localAddr）
	if c.localAddr == nil {
		return fmt.Errorf("local address not set (use NewConnection with localAddr)")
	}
	localUDPAddr, ok := c.localAddr.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("localAddr is not a UDP address")
	}

	// 创建UDP监听连接
	conn, err := net.ListenPacket("udp", localUDPAddr.String())
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", localUDPAddr, err)
	}

	// 初始化连接属性
	c.conn = conn
	c.localAddr = conn.LocalAddr() // 确认实际监听地址
	atomic.StoreInt32(&c.state, StateListening)

        c.mu.Unlock()

	// 启动工作协程
	go c.receiveWorker()
	go c.sendWorker()
	go c.retransmissionWorker()
	go c.keepAliveWorker()

	c.log.Info("Server listening", logger.String("local", c.localAddr.String()))

	// 等待客户端连接
	select {
	case <-c.listenChan:
		atomic.StoreInt32(&c.state, StateConnected)
		c.log.Info("Server accepted connection",
			logger.String("remote", c.remoteAddr.String()))
		return nil
	case <-time.After(DefaultTimeout):
		c.Close()
		return fmt.Errorf("listen timeout (local: %s)", c.localAddr)
	case <-c.ctx.Done():
		return fmt.Errorf("listen cancelled")
	}
}

// 发送数据到连接
func (c *Connection) Send(data []byte) error {
	if atomic.LoadInt32(&c.state) != StateConnected {
		return fmt.Errorf("connection not established")
	}

	// 必要时分片数据
	fragments := c.fragmentData(data)
	
	for _, fragment := range fragments {
		// 等待发送窗口可用
		if err := c.waitForSendWindow(uint32(len(fragment))); err != nil {
			return err
		}

		// 添加到发送缓冲区
		if err := c.sendBuffer.Write(fragment); err != nil {
			return fmt.Errorf("send buffer full: %w", err)
		}

		// 通知发送工作协程有数据待发送
		select {
		case c.sendReady <- struct{}{}:
		default:
			// 通道已满时忽略，避免阻塞
		}
	}

	return nil
}

// 从连接接收数据
func (c *Connection) Receive() ([]byte, error) {
	if atomic.LoadInt32(&c.state) != StateConnected {
		return nil, fmt.Errorf("connection not established")
	}

	for {
		select {
		case <-c.recvReady:
			// 直接读取可用数据，避免二次等待导致空读取
			n := c.receiveBuffer.Readable()
			if n > 0 {
				data, err := c.receiveBuffer.Read(n)
				if err != nil {
					return nil, err
				}
				// 防御：避免返回0字节数据
				if len(data) == 0 {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				return data, nil
			}
			// 若被唤醒但暂时没有数据（竞态），短暂让步后继续等待
			time.Sleep(5 * time.Millisecond)
		case <-c.ctx.Done():
			return nil, fmt.Errorf("connection closed")
		}
	}
}

// 带超时的接收方法
func (c *Connection) ReceiveWithTimeout(timeout time.Duration) ([]byte, error) {
        if atomic.LoadInt32(&c.state) != StateConnected {
                return nil, fmt.Errorf("connection not established")
        }
        deadline := time.NewTimer(timeout)
        defer deadline.Stop()
        for {
                select {
                case <-c.recvReady:
                        // 直接读取缓冲区可用数据，避免二次等待导致的空读
                        n := c.receiveBuffer.Readable()
                        if n > 0 {
                                data, err := c.receiveBuffer.Read(n)
                                if err != nil {
                                        return nil, err
                                }
                                // 防御：避免返回0字节数据
                                if len(data) == 0 {
                                        time.Sleep(5 * time.Millisecond)
                                        continue
                                }
                                return data, nil
                        }
                        // 若存在竞态导致暂时无数据，短暂让步后继续等待
                        time.Sleep(5 * time.Millisecond)
                case <-deadline.C:
                        return nil, fmt.Errorf("receive timeout")
                case <-c.ctx.Done():
                        return nil, fmt.Errorf("connection closed")
                }
        }
}

// 关闭连接
func (c *Connection) Close() error {
	// 检查并更新状态为关闭中
	if !c.compareAndSwapState(StateConnected, StateClosing) &&
		!c.compareAndSwapState(StateConnecting, StateClosing) {
		return nil // 已处于关闭或关闭中状态
	}

	c.log.Info("Closing connection")

	// 发送FIN包通知对方关闭连接
	c.sendFin()

	// 取消上下文
	c.cancel()

	// 关闭UDP连接
	if c.conn != nil {
		c.conn.Close()
	}

	// 更新状态为已关闭
	atomic.StoreInt32(&c.state, StateClosed)

	close(c.closeChan)

	c.log.Info("Connection closed",
		logger.Uint64("bytesSent", c.stats.BytesSent),
		logger.Uint64("bytesReceived", c.stats.BytesReceived))

	return nil
}

// 设置流量控制窗口大小
func (c *Connection) SetFlowControl(window uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sendWindow = window
	c.log.Debug("Flow control window updated", logger.Uint32("window", window))
}

// 返回连接的统计信息
func (c *Connection) GetStatistics() ConnectionStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.RTT = c.srtt
	return stats
}

// 内部方法

// sendSyn 发送SYN包发起连接
func (c *Connection) sendSyn() error {
	packet := &Packet{
		Type:      PacketTypeSyn,
		Sequence:  c.sendSeq,
		Window:    c.receiveWindow,
		Timestamp: uint32(time.Now().Unix()),
	}

	return c.sendPacket(packet)
}

// sendFin 发送FIN包关闭连接
func (c *Connection) sendFin() error {
	packet := &Packet{
		Type:      PacketTypeFin,
		Sequence:  c.sendSeq,
		Timestamp: uint32(time.Now().Unix()),
	}

	return c.sendPacket(packet)
}

// sendAck 发送确认包（回显对端数据包的时间戳用于 RTT 计算）
func (c *Connection) sendAckPacket(seq uint32, echoTS uint32) error {
	packet := &Packet{
		Type:      PacketTypeAck,
		Sequence:  c.sendSeq,           // 携带本端当前序列号（用于对端学习本端ISN）
		Ack:       seq,
		Window:    c.receiveWindow,
		Timestamp: echoTS,              // 回显对端发送时间戳用于RTT
	}

	return c.sendPacket(packet)
}

 // sendPacket 发送数据包
func (c *Connection) sendPacket(packet *Packet) error {
	if c.conn == nil {
                return fmt.Errorf("connection not initialized")
        }

	// 避免发送空数据包
	if packet.Type == PacketTypeData {
		c.log.Debug("TX Data",
			logger.Uint32("seq", packet.Sequence),
			logger.Int("len", len(packet.Data)),
		)
		if len(packet.Data) == 0 {
			c.log.Warn("Skip sending empty data packet", logger.Uint32("seq", packet.Sequence))
			return nil
		}
	}
	data := c.encodePacket(packet)
	
	// 通过UDP发送
	_, err := c.conn.WriteTo(data, c.remoteAddr)
	if err != nil {
		return err
	}

	// 更新统计信息
	atomic.AddUint64(&c.stats.PacketsSent, 1)
	atomic.AddUint64(&c.stats.BytesSent, uint64(len(data)))

	// 非ACK/保活包添加到重传队列
        if packet.Type != PacketTypeAck && packet.Type != PacketTypeKeepAlive {
                c.retransQueue.Add(packet.Sequence, data, time.Now().UnixMilli())
        }

	return nil
}

// encodePacket 编码数据包为字节流
// 注意：这是简化实现，实际应用中应使用更完善的编码方式
func (c *Connection) encodePacket(packet *Packet) []byte {
	// 头部固定22字节：Type(1)+Flags(1)+Sequence(4)+Ack(4)+Window(4)+Timestamp(4)+Checksum(4)
        headerSize := 22
        totalSize := headerSize + len(packet.Data)
        buf := make([]byte, totalSize) // 缓冲区长度 = 22 + 数据长度

        // 填充头部
        buf[0] = packet.Type
        buf[1] = packet.Flags
        copy(buf[2:6], uint32ToBytes(packet.Sequence))   // Sequence(4字节)
        copy(buf[6:10], uint32ToBytes(packet.Ack))      // Ack(4字节)
        copy(buf[10:14], uint32ToBytes(packet.Window))  // Window(4字节)
        copy(buf[14:18], uint32ToBytes(packet.Timestamp))// Timestamp(4字节)
        copy(buf[18:22], uint32ToBytes(packet.Checksum))// Checksum(4字节，暂不实现校验和计算)

        // 填充数据
        if len(packet.Data) > 0 {
                copy(buf[headerSize:], packet.Data)
        }

        return buf
}

// decodePacket 从字节流解码出数据包
func (c *Connection) decodePacket(data []byte) (*Packet, error) {
	if len(data) < 22 {
                return nil, fmt.Errorf("packet too small (min 22 bytes)")
        }

        packet := &Packet{
                Type:      data[0],
                Flags:     data[1],
                Sequence:  bytesToUint32(data[2:6]),
                Ack:       bytesToUint32(data[6:10]),
                Window:    bytesToUint32(data[10:14]),
                Timestamp: bytesToUint32(data[14:18]),
                Checksum:  bytesToUint32(data[18:22]),
        }

        // 提取数据部分
        if len(data) > 22 {
                packet.Data = data[22:]
        }

        return packet, nil
}

// fragmentData 将数据分块为适合传输的大小
func (c *Connection) fragmentData(data []byte) [][]byte {
	maxSize := int(c.congestionWnd)
	if maxSize > DefaultMTU {
		maxSize = DefaultMTU
	}

	var fragments [][]byte
	for len(data) > 0 {
		size := len(data)
		if size > maxSize {
			size = maxSize
		}
		fragments = append(fragments, data[:size])
		data = data[size:]
	}

	return fragments
}

// waitForSendWindow 等待发送窗口可用
// TODO: 实现带阻塞的完善流量控制
func (c *Connection) waitForSendWindow(size uint32) error {
	// 检查窗口是否有足够空间
        if c.sendBuffer.Available() < int(size) {
                // 等待窗口更新（实际应添加超时机制）
                select {
                case <-c.ctx.Done():
                        return fmt.Errorf("connection closed while waiting for window")
                case <-time.After(DefaultTimeout):
                        return fmt.Errorf("timeout waiting for send window")
                }
        }
        return nil
}

// compareAndSwapState 原子地比较并交换连接状态
func (c *Connection) compareAndSwapState(old, new int32) bool {
	return atomic.CompareAndSwapInt32(&c.state, old, new)
}

// 工作协程

// receiveWorker 接收并处理数据包的协程
func (c *Connection) receiveWorker() {
	buffer := make([]byte, 65536) // 接收缓冲区
	
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// 设置读取超时
			if deadline, ok := c.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
				deadline.SetReadDeadline(time.Now().Add(1 * time.Second))
			}

			// 读取数据包
			n, remoteAddr, err := c.conn.ReadFrom(buffer)
			if err != nil {
				// 连接关闭或上下文取消：静默退出
				if c.ctx.Err() != nil {
					return
				}
				// 读超时：继续循环
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				// 套接字已关闭：静默退出
				if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				// 其他错误：记录日志并继续
				c.log.Error("Receive error", zap.Error(err))
				continue
			}

			// 服务端首次接收时记录客户端地址
                        if atomic.LoadInt32(&c.state) == StateListening && c.remoteAddr == nil {
                                c.remoteAddr = remoteAddr
                        }

			// 验证地址（已连接状态下只接收来自远程地址的包）
                        if atomic.LoadInt32(&c.state) == StateConnected &&
                                remoteAddr.String() != c.remoteAddr.String() {
                                c.log.Warn("Received packet from unknown address",
                                        logger.String("expected", c.remoteAddr.String()),
                                        logger.String("received", remoteAddr.String()))
                                continue
                        }

			// 解码数据包
			packet, err := c.decodePacket(buffer[:n])
			if err != nil {
				c.log.Error("Decode error", zap.Error(err))
				continue
			}

			// 处理数据包
			c.processPacket(packet)

			// 更新统计信息
			atomic.AddUint64(&c.stats.PacketsReceived, 1)
			atomic.AddUint64(&c.stats.BytesReceived, uint64(n))
		}
	}
}

// sendWorker 处理待发送数据的协程
func (c *Connection) sendWorker() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.sendReady:
			// 发送待处理数据
			c.sendPendingData()
		}
	}
}

// retransmissionWorker 处理重传的协程
func (c *Connection) retransmissionWorker() {
	ticker := time.NewTicker(100 * time.Millisecond) // 每100ms检查一次
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.checkRetransmissions()
		}
	}
}

// keepAliveWorker 处理保活的协程
func (c *Connection) keepAliveWorker() {
	ticker := time.NewTicker(DefaultKeepAlive) // 按默认保活间隔发送
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if atomic.LoadInt32(&c.state) == StateConnected {
				// 检查最后活动时间，超过保活间隔则发送保活包
                                if time.Since(c.lastActivity) > DefaultKeepAlive {
                                        c.sendKeepAlive()
                                }
			}
		}
	}
}

// processPacket 处理接收到的数据包
func (c *Connection) processPacket(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastActivity = time.Now() // 更新最后活动时间

	// 根据包类型处理
	switch packet.Type {
	case PacketTypeData:
		c.handleDataPacket(packet)
	case PacketTypeAck:
		c.handleAckPacket(packet)
	case PacketTypeSyn:
		c.handleSynPacket(packet)
	case PacketTypeFin:
		c.handleFinPacket(packet)
	case PacketTypeKeepAlive:
		c.handleKeepAlivePacket(packet)
	case PacketTypeWindowUpdate:
		c.handleWindowUpdate(packet)
	default:
                c.log.Warn("Unknown packet type", logger.Uint8("type", packet.Type))
	}
}

// handleDataPacket 处理数据报文
func (c *Connection) handleDataPacket(packet *Packet) {
	c.log.Debug("RX Data",
		logger.Uint32("seq", packet.Sequence),
		logger.Uint32("expected", c.receiveSeq),
		logger.Int("len", len(packet.Data)),
	)
	// 忽略空数据包：不推进序列，回显当前累计ACK，避免状态卡死
	if len(packet.Data) == 0 {
		c.sendAckPacket(c.receiveSeq, packet.Timestamp)
		return
	}
	// 检查序列号是否合法（简化：只处理连续的序列号）
        if packet.Sequence != c.receiveSeq {
                c.log.Warn("Out-of-order data packet", 
                        logger.Uint32("expected", c.receiveSeq),
                        logger.Uint32("received", packet.Sequence))
                // 发送重复ACK
                c.sendAckPacket(c.receiveSeq, packet.Timestamp)
                c.stats.DuplicateAcks++
                return
        }

        // 写入接收缓冲区
        if err := c.receiveBuffer.Write(packet.Data); err != nil {
                c.log.Warn("Receive buffer full", zap.Error(err))
                return
        }

        // 更新接收序列号
        c.receiveSeq += uint32(len(packet.Data))
        // 发送ACK确认
        c.sendAckPacket(c.receiveSeq, packet.Timestamp)

        // 通知应用层有数据
        select {
        case c.recvReady <- struct{}{}:
        default:
        }
}

// handleAckPacket 处理确认报文
func (c *Connection) handleAckPacket(packet *Packet) {
	// 调试：记录收到的 ACK 及当前确认进度
	oldSendAck := c.sendAck
	c.log.Debug("RX ACK",
		logger.Uint32("ack", packet.Ack),
		logger.Uint32("oldSendAck", oldSendAck),
		logger.Int("dupAck", c.dupAckCount),
	)
	// 更新RTT（使用ACK中回显的对端发送时间戳）
	sendTime := time.Unix(int64(packet.Timestamp), 0)
	rtt := time.Since(sendTime)
	c.updateRTT(rtt)

	// 基于累计ACK进行裁切，保留未确认尾部
	c.retransQueue.TrimUpTo(packet.Ack)

	// 处理ACK前进/重复ACK（快速重传）
	if packet.Ack > c.sendAck {
		// ACK 前进：更新确认号，重置重复计数，并调整拥塞窗口
		c.sendAck = packet.Ack
		c.dupAckCount = 0
		c.lastAck = packet.Ack
		c.updateCongestionWindow(true)
		// ACK前进释放了窗口，立刻尝试发送以“补洞”，避免等待 sendReady 才发送
		c.sendPendingData()
	} else if packet.Ack == c.sendAck {
		// 重复ACK：计数+1，达到3次触发快速重传最早未确认片段
		c.dupAckCount++
		c.stats.DuplicateAcks++
		if c.dupAckCount >= 3 {
			if entry := c.retransQueue.PeekEarliest(); entry != nil && entry.Sequence >= packet.Ack {
				// 立即重传该片段
				if _, err := c.conn.WriteTo(entry.Data, c.remoteAddr); err == nil {
					entry.Attempts++
					entry.NextRetrans = time.Now().Add(c.rto).UnixMilli()
					c.retransQueue.Update(entry)
					// 拥塞控制进入恢复（简单处理：视为丢包一次）
					c.updateCongestionWindow(false)
				}
			}
			// 重置重复计数以避免频繁触发
			c.dupAckCount = 0
		}
	} else {
		// 旧ACK：忽略
	}

	// 通知连接建立（如果是SYN-ACK）
	if atomic.LoadInt32(&c.state) == StateConnecting {
		// 学习对端ISN：ACK包的 Sequence 字段为对端当前序列号（ACK不消耗序号）
		c.receiveSeq = packet.Sequence
		select {
		case c.ackChan <- packet.Ack:
		default:
		}
	}
}

// handleSynPacket 处理同步报文（服务端逻辑）
func (c *Connection) handleSynPacket(packet *Packet) {
	// 只有监听状态的服务端处理SYN
        if atomic.LoadInt32(&c.state) != StateListening {
                return
        }

        // 初始化服务端序列号（随机值，这里简化为2000）
        c.sendSeq = 2000
        // 回复SYN-ACK：确认号=客户端SYN序列号+1
        c.sendAckPacket(packet.Sequence + 1, packet.Timestamp)
	// 更新接收确认号（客户端下一个应发送的序列号）
        c.receiveAck = packet.Sequence + 1
        // 初始化接收序列号（期望客户端下一个数据序列）
        c.receiveSeq = packet.Sequence + 1
        // 通知Listen方法连接建立（只发送一次信号）
        select {
        case c.listenChan <- struct{}{}:
        default:
        }
}

// handleFinPacket 处理结束报文
func (c *Connection) handleFinPacket(packet *Packet) {
	// 发送FIN-ACK响应
	c.sendAckPacket(packet.Sequence, packet.Timestamp)
	
	// 关闭连接
	c.Close()
}

// 处理保活报文（回复ACK即可）
func (c *Connection) handleKeepAlivePacket(packet *Packet) {
        // KeepAlive 不携带数据也不消耗序号，确认应基于当前累计接收进度
        c.sendAckPacket(c.receiveSeq, packet.Timestamp)
}

// handleWindowUpdate 处理窗口更新报文
func (c *Connection) handleWindowUpdate(packet *Packet) {
	c.sendWindow = packet.Window
	c.stats.WindowUpdates++
	c.log.Debug("Received window update", logger.Uint32("new window", packet.Window))
}

 // sendPendingData 发送缓冲区中的待发送数据（基于滑动窗口：限制未确认字节数）
func (c *Connection) sendPendingData() {
        if atomic.LoadInt32(&c.state) != StateConnected {
                return
        }
        for {
                // 无数据或对端窗口为0则退出
                if c.sendBuffer.Readable() == 0 || c.sendWindow == 0 {
                        break
                }

                // 计算已发送未确认的字节数 inFlight = sendSeq - sendAck
                inFlight := c.sendSeq - c.sendAck

                // 本次允许的最大发送量 = min(对端窗口, 拥塞窗口) - inFlight
                cwnd := c.sendWindow
                if c.congestionWnd < cwnd {
                        cwnd = c.congestionWnd
                }
                if inFlight >= cwnd {
                        // 已达到窗口上限，等待ACK推进
                        break
                }
                // 允许发送的剩余窗口
                allowance := int(cwnd - inFlight)

                // 实际读取大小：不超过剩余窗口、不超过缓冲区可用、不超过单包上限
                readSize := allowance
                if readSize > c.sendBuffer.Readable() {
                        readSize = c.sendBuffer.Readable()
                }
                if readSize > DefaultMTU {
                        readSize = DefaultMTU
                }
                if readSize <= 0 {
                        break
                }

                data, err := c.sendBuffer.Read(readSize)
                if err != nil {
                        c.log.Error("Failed to read send buffer", zap.Error(err))
                        break
                }
                // 不要发送空数据片段
                if len(data) == 0 {
                        break
                }

                // 构造数据报文并发送（Ack 使用当前累计接收进度）
                packet := &Packet{
                        Type:      PacketTypeData,
                        Sequence:  c.sendSeq,
                        Ack:       c.receiveSeq,
                        Window:    c.receiveWindow,
                        Timestamp: uint32(time.Now().Unix()),
                        Data:      data,
                }
                if err := c.sendPacket(packet); err != nil {
                        c.log.Error("Failed to send data packet", zap.Error(err))
                        // 数据放回缓冲区
                        c.sendBuffer.Unread(data)
                        break
                }

                // 递增发送序列号
                c.sendSeq += uint32(len(data))
        }
}

// checkRetransmissions 检查并处理需要重传的数据包
func (c *Connection) checkRetransmissions() {
	now := time.Now()
        // 获取所有超时未确认的包
        packets := c.retransQueue.GetExpired(now.UnixMilli())

        for _, p := range packets {
                // 检查重传次数
                if p.Attempts >= MaxRetransmissions {
                        c.log.Error("Max retransmissions reached, closing connection",
                                logger.Uint32("sequence", p.Sequence))
                        c.Close()
                        return
                }

                // 重传数据包
                _, err := c.conn.WriteTo(p.Data, c.remoteAddr)
                if err != nil {
                        c.log.Error("Failed to retransmit packet", 
                                logger.Uint32("sequence", p.Sequence),
                                zap.Error(err))
                        continue
                }

                // 更新统计和重传信息
                c.stats.Retransmissions++
                p.Attempts++
                p.NextRetrans = now.Add(c.rto * (1 << p.Attempts)).UnixMilli() // 指数退避
                c.retransQueue.Update(p)

                c.log.Debug("Retransmitted packet",
                        logger.Uint32("sequence", p.Sequence),
                        logger.Int("retries", p.Attempts))
        }
}

// sendKeepAlive 发送保活包
func (c *Connection) sendKeepAlive() {
	packet := &Packet{
                Type:      PacketTypeKeepAlive,
                Sequence:  c.sendSeq, // 复用当前序列号（不携带数据，不递增）
                Ack:       c.receiveSeq,
                Timestamp: uint32(time.Now().Unix()),
        }
        c.sendPacket(packet)
}

// updateRTT 更新RTT(往返时间)估计
// 基于RFC 6298标准
func (c *Connection) updateRTT(rtt time.Duration) {
	// 更新平滑RTT (RFC 6298)
	if c.srtt == 0 {
		// 首次测量
		c.srtt = rtt
		c.rttvar = rtt / 2
	} else {
		alpha := 0.125
		beta := 0.25
		
		// 更新RTT方差
		c.rttvar = time.Duration((1-beta)*float64(c.rttvar) + beta*float64(abs(c.srtt-rtt)))
		// 更新平滑RTT
		c.srtt = time.Duration((1-alpha)*float64(c.srtt) + alpha*float64(rtt))
	}

	// 更新RTO(重传超时时间)
	c.rto = c.srtt + 4*c.rttvar
	// 确保RTO在合理范围内
	if c.rto < MinRTO {
		c.rto = MinRTO
	} else if c.rto > MaxRTO {
		c.rto = MaxRTO
	}

	c.stats.RTT = c.srtt
}

// updateCongestionWindow 更新拥塞窗口
// ack: 是否是确认包(用于判断是增加还是减少窗口)
func (c *Connection) updateCongestionWindow(ack bool) {
	if ack {
		// 收到确认，增加窗口(慢启动或拥塞避免)
		if c.congestionWnd < c.ssthresh {
			// 慢启动阶段：每个ACK增加一个MSS
			c.congestionWnd += DefaultMTU
		} else {
			// 拥塞避免阶段：线性增加
			c.congestionWnd += DefaultMTU * DefaultMTU / c.congestionWnd
		}
		// 窗口不超过最大发送窗口
                if c.congestionWnd > c.sendWindow {
                        c.congestionWnd = c.sendWindow
                }
	} else {
		// 发生丢包，减少窗口
		c.ssthresh = c.congestionWnd / 2
		c.congestionWnd = DefaultMTU * 2
		c.stats.Retransmissions++
	}
}

// LocalAddr 返回连接的本地地址
func (c *Connection) LocalAddr() net.Addr {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.localAddr
}

// RemoteAddr 返回连接的远程地址
func (c *Connection) RemoteAddr() net.Addr {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.remoteAddr
}

// 辅助函数：时间间隔绝对值
func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// 辅助函数：uint32转字节数组（大端序）
func uint32ToBytes(n uint32) []byte {
        return []byte{
                byte(n >> 24),
                byte(n >> 16),
                byte(n >> 8),
                byte(n),
        }
}

// 辅助函数：字节数组转uint32（大端序）
func bytesToUint32(b []byte) uint32 {
        return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

