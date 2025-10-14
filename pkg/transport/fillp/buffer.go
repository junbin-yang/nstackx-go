// 提供FILLP协议的缓冲区管理功能
package fillp

import (
	"fmt"
	"sync"
	"time"
)

// FILLP数据的循环缓冲区（环形缓冲区）
// 用于高效地进行数据的读写操作，适合生产-消费模型
type RingBuffer struct {
	mu sync.Mutex // 互斥锁，保证线程安全

	data     []byte // 存储数据的缓冲区
	size     int    // 缓冲区总容量
	head     int    // 读指针，指向当前可读取数据的位置
	tail     int    // 写指针，指向当前可写入数据的位置
	count    int    // 当前缓冲区中的数据量
	isClosed bool   // 缓冲区是否已关闭
}

// 创建一个新的环形缓冲区
// 参数size指定缓冲区的容量大小
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		data: make([]byte, size),
		size: size,
	}
}

// 向缓冲区写入数据
// 如果缓冲区已关闭或空间不足，返回错误
func (rb *RingBuffer) Write(data []byte) error {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.isClosed {
		return fmt.Errorf("buffer is closed")
	}

	// 检查是否有足够空间
	if len(data) > rb.available() {
		return fmt.Errorf("insufficient space: need %d, have %d", len(data), rb.available())
	}

	// 写入数据并移动写指针
	for _, b := range data {
		rb.data[rb.tail] = b
		rb.tail = (rb.tail + 1) % rb.size // 循环移动指针
		rb.count++
	}

	return nil
}

// 从缓冲区读取指定大小的数据
// 如果数据不足，返回所有可用数据
func (rb *RingBuffer) Read(size int) ([]byte, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.isClosed {
		return nil, fmt.Errorf("buffer is closed")
	}

	// 调整读取大小为实际可用数据量
	if size > rb.count {
		size = rb.count
	}

	if size == 0 {
		return nil, nil
	}

	// 读取数据并移动读指针
	result := make([]byte, size)
	for i := 0; i < size; i++ {
		result[i] = rb.data[rb.head]
		rb.head = (rb.head + 1) % rb.size // 循环移动指针
		rb.count--
	}

	return result, nil
}

// 查看缓冲区中的数据但不将其从缓冲区中移除
// 与Read类似，但不会修改读指针和数据计数
func (rb *RingBuffer) Peek(size int) ([]byte, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.isClosed {
		return nil, fmt.Errorf("buffer is closed")
	}

	// 调整查看大小为实际可用数据量
	if size > rb.count {
		size = rb.count
	}

	if size == 0 {
		return nil, nil
	}

	// 读取数据但不修改原始头指针
	result := make([]byte, size)
	head := rb.head // 使用临时头指针
	for i := 0; i < size; i++ {
		result[i] = rb.data[head]
		head = (head + 1) % rb.size
	}

	return result, nil
}

// 返回可写入的字节数
func (rb *RingBuffer) Available() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.available()
}

// 将数据放回缓冲区（用于发送失败时回退）
func (rb *RingBuffer) Unread(data []byte) {
        rb.mu.Lock()
        defer rb.mu.Unlock()

        for i := len(data) - 1; i >= 0; i-- {
                rb.head = (rb.head - 1 + len(rb.data)) % len(rb.data)
                rb.data[rb.head] = data[i]
                rb.count++
        }
}

// 内部方法，返回可写入的字节数（不加锁）
func (rb *RingBuffer) available() int {
	return rb.size - rb.count
}

 // 返回当前缓冲区中的数据字节数
func (rb *RingBuffer) Used() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// 返回当前可读字节数
func (rb *RingBuffer) Readable() int {
	return rb.Used()
}

// 清空缓冲区中的所有数据
func (rb *RingBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.head = 0
	rb.tail = 0
	rb.count = 0
}

// 关闭缓冲区，之后无法进行读写操作
func (rb *RingBuffer) Close() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.isClosed = true
}

// 返回缓冲区是否已关闭
func (rb *RingBuffer) IsClosed() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.isClosed
}

// 管理等待确认的数据包
// 用于实现可靠传输，跟踪需要重传的数据包
type RetransmissionQueue struct {
	mu sync.Mutex // 互斥锁，保证线程安全

	packets map[uint32]*RetransmissionEntry // 按序列号存储的数据包
	timer   map[uint32]*RetransmissionTimer // 按序列号存储的重传计时器
}

// 表示等待确认的数据包条目
type RetransmissionEntry struct {
	Sequence    uint32 // 数据包序列号
	Data        []byte // 数据包内容
	Timestamp   int64  // 首次发送时间戳
	Attempts    int    // 重传次数
	NextRetrans int64  // 下次重传时间戳
}

// 跟踪重传超时信息
type RetransmissionTimer struct {
	Sequence uint32 // 对应数据包的序列号
	Timeout  int64  // 超时时间戳
	Attempts int    // 已尝试重传次数
}

// 创建一个新的重传队列
func NewRetransmissionQueue() *RetransmissionQueue {
	return &RetransmissionQueue{
		packets: make(map[uint32]*RetransmissionEntry),
		timer:   make(map[uint32]*RetransmissionTimer),
	}
}

// 向重传队列添加一个数据包
// seq: 序列号, data: 数据内容, timestamp: 发送时间戳
func (rq *RetransmissionQueue) Add(seq uint32, data []byte, timestamp int64) {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	// 添加数据包条目，初始重传时间（降低以适配回环场景更快恢复）
	rq.packets[seq] = &RetransmissionEntry{
		Sequence:    seq,
		Data:        data,
		Timestamp:   timestamp,
		Attempts:    0,
		NextRetrans: timestamp + 200, // 初始重传超时(RTO): 200ms
	}

	// 添加对应的计时器
	rq.timer[seq] = &RetransmissionTimer{
		Sequence: seq,
		Timeout:  timestamp + 200,
		Attempts: 0,
	}
}

// 从队列中移除指定序列号的数据包（已确认）
// 返回是否成功移除
func (rq *RetransmissionQueue) Remove(seq uint32) bool {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	if _, exists := rq.packets[seq]; exists {
		delete(rq.packets, seq)
		delete(rq.timer, seq)
		return true
	}
	return false
}

func (rq *RetransmissionQueue) RemoveUpTo(seq uint32) {
        rq.mu.Lock()
        defer rq.mu.Unlock()
        for s := range rq.packets {
                if s < seq {
                        delete(rq.packets, s)
                }
        }
}

// 返回需要重传的数据包（已超时未确认）
// now: 当前时间戳
func (rq *RetransmissionQueue) GetExpired(now int64) []*RetransmissionEntry {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	var expired []*RetransmissionEntry
	for _, entry := range rq.packets {
		if entry.NextRetrans <= now {
			// 仅收集到期的条目，不在此处修改 Attempts/NextRetrans
			// 由上层（connection.go 的 checkRetransmissions）统一处理退避与计数
			expired = append(expired, entry)
		}
	}
	return expired
}

func (rq *RetransmissionQueue) Update(p *RetransmissionEntry) {
        rq.mu.Lock()
        defer rq.mu.Unlock()
        rq.packets[p.Sequence] = p
}

// PeekEarliest 返回队列中最早(最小序列号)的未确认条目；若为空返回 nil
func (rq *RetransmissionQueue) PeekEarliest() *RetransmissionEntry {
        rq.mu.Lock()
        defer rq.mu.Unlock()

        var minSeq uint32 = ^uint32(0)
        var minEntry *RetransmissionEntry
        for seq, e := range rq.packets {
                if seq < minSeq {
                        minSeq = seq
                        minEntry = e
                }
        }
        return minEntry
}

// TrimUpTo 裁切已累计确认到 ack 的字节：
// - 对于完全覆盖的包(包尾 <= ack)直接删除
// - 对于部分覆盖的包(包头 < ack < 包尾)裁掉前段，并将剩余未确认部分作为新条目，Sequence=ack
func (rq *RetransmissionQueue) TrimUpTo(ack uint32) {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	// 按序（升序）处理，优先对齐队头，避免 map 无序遍历引发遗漏或错位
	keys := make([]uint32, 0, len(rq.packets))
	for k := range rq.packets {
		keys = append(keys, k)
	}
	// 手动插入排序避免额外依赖
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}

	for _, seq := range keys {
		entry, ok := rq.packets[seq]
		if !ok {
			continue
		}
		// 仅处理完整编码帧；且仅对数据包做按负载裁切
		if len(entry.Data) < 22 || entry.Data[0] != PacketTypeData {
			// 非数据包（例如 SYN/FIN 无负载）：用序号规则删除
			if seq < ack {
				delete(rq.packets, seq)
				if _, ok := rq.timer[seq]; ok {
					delete(rq.timer, seq)
				}
			}
			continue
		}

		payloadLen := len(entry.Data) - 22
		if payloadLen <= 0 {
			// 无负载：用序号规则删除
			if seq < ack {
				delete(rq.packets, seq)
				if _, ok := rq.timer[seq]; ok {
					delete(rq.timer, seq)
				}
			}
			continue
		}

		end := seq + uint32(payloadLen)

		// 完全确认：删除
		if end <= ack {
			delete(rq.packets, seq)
			if _, ok := rq.timer[seq]; ok {
				delete(rq.timer, seq)
			}
			continue
		}

		// 队头未覆盖：后续更大序号也不会覆盖，提前结束
		if ack <= seq {
			break
		}

		// 部分确认：裁切前段（基于负载），重写帧头 Sequence=ack
		trim := int(ack - seq)

		header := make([]byte, 22)
		copy(header, entry.Data[:22])
		copy(header[2:6], uint32ToBytes(ack)) // 重写 Sequence

		remain := entry.Data[22+trim:]
		newData := append(header, remain...)

		// 从旧键删除并把条目重建到新键 ack
		delete(rq.packets, seq)
		if t, ok := rq.timer[seq]; ok {
			delete(rq.timer, seq)
			t.Sequence = ack
			t.Attempts = 0
			t.Timeout = time.Now().Add(200 * time.Millisecond).UnixMilli()
			rq.timer[ack] = t
		}

		entry.Sequence = ack
		entry.Data = newData
		entry.Attempts = 0
		entry.NextRetrans = time.Now().Add(200 * time.Millisecond).UnixMilli()
		rq.packets[ack] = entry

		// 队头已与 ack 对齐，裁切一次即可
		break
	}
}

// 返回队列中的数据包数量
func (rq *RetransmissionQueue) Size() int {
	rq.mu.Lock()
	defer rq.mu.Unlock()
	return len(rq.packets)
}

// 清空队列中的所有数据包
func (rq *RetransmissionQueue) Clear() {
	rq.mu.Lock()
	defer rq.mu.Unlock()

	rq.packets = make(map[uint32]*RetransmissionEntry)
	rq.timer = make(map[uint32]*RetransmissionTimer)
}

