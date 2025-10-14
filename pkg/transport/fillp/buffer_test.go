package fillp

import (
	"testing"
	"time"
	"fmt"
)

// 环形缓冲区（RingBuffer）适用于 “生产 - 消费” 场景（如发送端缓存待发数据、接收端缓存已收数据）
// 以下示例模拟 “数据写入→查看→读取→清空” 全流程。
// 初始化：通过NewRingBuffer(size int)创建实例，size建议设为业务中单次最大数据量的 2-3 倍，避免频繁触发 “空间不足” 错误。
// 写入限制：Write()会检查剩余空间，若待写入数据长度超过Available()，返回错误，需在业务中处理（如等待或扩容）。
// 并发安全：组件内部通过sync.Mutex保证线程安全，多协程读写无需额外加锁。
func Test_RingBuffer(t *testing.T) {
	fmt.Println("[RingBuffer]")

	// 1. 初始化环形缓冲区：指定容量为1024字节（根据业务需求调整）
	buffer := NewRingBuffer(1024)
	if buffer == nil {
                fmt.Println("环形缓冲区初始化失败")
                return
        }
	fmt.Println("环形缓冲区初始化成功，容量：1024字节")

	// 2. 向缓冲区写入数据（模拟生产数据）
	testData := []byte("FILLP reliable data transmission test")
	err := buffer.Write(testData)
        if err != nil {
                fmt.Printf("写入缓冲区失败：%v\n", err)
                return
        }
        fmt.Printf("写入缓冲区成功，数据长度：%d字节\n", len(testData))

	// 3. 查看缓冲区数据（不删除数据，仅预览）
        peekSize := 20  // 查看前20字节
        peekData, err := buffer.Peek(peekSize)
        if err != nil {
                fmt.Printf("查看缓冲区数据失败：%v\n", err)
                return
        }
        fmt.Printf("查看缓冲区前%d字节：%s\n", peekSize, string(peekData))

	// 4. 读取缓冲区数据（读取后数据从缓冲区移除，模拟消费数据）
        readSize := len(testData)  // 读取全部写入数据
        readData, err := buffer.Read(readSize)
        if err != nil {
                fmt.Printf("读取缓冲区数据失败：%v\n", err)
                return
        }
        fmt.Printf("读取缓冲区数据成功，数据内容：%s\n", string(readData))

	// 5. 查询缓冲区状态（已用空间、可用空间）
        used := buffer.Used()
        available := buffer.Available()
        fmt.Printf("读取后缓冲区状态：已用空间%d字节，可用空间%d字节\n", used, available)

	// 6. 清空缓冲区（如需复用缓冲区）
        buffer.Clear()
        fmt.Println("缓冲区清空成功，当前已用空间：", buffer.Used())

        // 7. 关闭缓冲区（不再使用时调用，避免资源泄漏）
        buffer.Close()
        fmt.Println("缓冲区已关闭，是否关闭：", buffer.IsClosed())
}

// 重传队列（RetransmissionQueue）用于跟踪已发送但未确认的数据包，自动检测超时并触发重传
// 以下示例模拟 “添加数据包→检测超时→重传→移除确认包” 流程。
// 序列号规则：Add(seq uint32, ...)中的seq需全局唯一（如自增整数），确保数据包可被唯一标识和确认。
// 指数退避：超时重传间隔按 “1s→2s→4s→8s...” 翻倍（1000 << entry.Attempts），避免网络拥塞加剧，符合 RFC 标准。
// 超时检测：建议通过定时任务（如每 100ms）调用GetExpired(now)，及时发现超时数据包。
func Test_RetransmissionQueue(t *testing.T) {
	fmt.Println("[RetransmissionQueue]")

	// 1. 初始化重传队列
        rq := NewRetransmissionQueue()
	if rq == nil {
                fmt.Println("重传队列初始化失败")
                return
        }
        fmt.Println("重传队列初始化成功")

	// 2. 模拟发送数据包，添加到重传队列（序列号自增，时间戳用当前毫秒数）
        now := time.Now().UnixMilli()  // 获取当前时间戳（毫秒）
        packet1 := []byte("FILLP retransmission test packet 1")
        packet2 := []byte("FILLP retransmission test packet 2")

	// 添加2个数据包（序列号1001、1002，模拟业务中的唯一标识）
        rq.Add(1001, packet1, now)
        rq.Add(1002, packet2, now)
        fmt.Printf("添加2个数据包到重传队列，当前队列大小：%d\n", rq.Size())

	// 3. 模拟时间流逝（等待1.5秒，超过初始重传超时1秒）
        fmt.Println("等待1.5秒，检测超时数据包...")
        time.Sleep(1500 * time.Millisecond)
        nowAfterWait := time.Now().UnixMilli()

	// 4. 获取超时需重传的数据包
        expiredPackets := rq.GetExpired(nowAfterWait)
        fmt.Printf("检测到超时数据包数量：%d\n", len(expiredPackets))
        for _, pkt := range expiredPackets {
                fmt.Printf("需重传数据包：序列号=%d，已重传次数=%d，下次重传时间=%d毫秒\n",
                        pkt.Sequence, pkt.Attempts, pkt.NextRetrans)
                // 此处可添加实际重传逻辑（如调用发送接口重新发送pkt.Data）
        }

	// 5. 模拟收到数据包确认，从队列中移除（如收到序列号1001的确认）
        removed := rq.Remove(1001)
        if removed {
                fmt.Println("序列号1001的数据包已确认，从队列中移除")
        } else {
                fmt.Println("序列号1001的数据包未找到，移除失败")
        }
        fmt.Printf("移除后队列大小：%d\n", rq.Size())

        // 6. 清空重传队列（如连接关闭时）
        rq.Clear()
        fmt.Println("重传队列清空成功，当前队列大小：", rq.Size())
}

