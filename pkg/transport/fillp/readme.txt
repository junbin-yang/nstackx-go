FILLP 协议包可靠数据传输组件调用示例文档

本文档针对 FILLP 协议包中环形缓冲区（RingBuffer）、重传队列（RetransmissionQueue）、连接（Connection） 三大核心可靠传输组件，提供完整调用示例，覆盖组件初始化、核心操作、异常处理等场景，可直接参考集成到业务代码中。

一、组件说明与依赖

组件名称		核心作用										依赖关系
RingBuffer		线程安全的环形缓冲区，用于临时存储待发送 / 接收的字节数据，解决数据读写速率不匹配问题	无外部依赖，独立使用
RetransmissionQueue	管理待确认数据包，实现超时检测与指数退避重传，保障数据不丢失				依赖RetransmissionEntry和RetransmissionTimer结构体
Connection		封装 FILLP 协议完整连接生命周期，整合缓冲区与重传队列，提供端到端可靠数据传输接口	依赖前两个组件，依赖net包实现 UDP 底层通信

二、环形缓冲区（RingBuffer）调用示例

RingBuffer适用于 “生产 - 消费” 场景（如发送端缓存待发数据、接收端缓存已收数据）

执行测试用例[buffer_test.go:Test_RingBuffer]：go test

三、重传队列（RetransmissionQueue）调用示例

RetransmissionQueue用于跟踪已发送但未确认的数据包，自动检测超时并触发重传。

执行测试用例[buffer_test.go:Test_RetransmissionQueue]：go test

四、FILLP 连接（Connection）调用示例

Connection是可靠传输的核心封装，整合缓冲区与重传队列，提供 “连接建立→数据收发→连接关闭” 全流程接口，以下示例模拟客户端与服务端的端到端通信。

1. 服务端示例（接收数据）

go run examples/fillp_server.go

2. 客户端示例（发送数据）

go run examples/fillp_client.go

3. 关键操作说明

连接建立：客户端通过Connect()发起连接，服务端需先启动并监听对应地址，底层通过 SYN/SYN-ACK 握手确认。
数据收发：Send(data []byte)自动处理数据分片（按 MTU 大小）和缓存，Receive()阻塞获取数据（需通过超时控制避免无限阻塞）。
连接关闭：通过Close()发送 FIN 包，释放缓冲区、重传队列和 UDP 资源，建议通过defer确保调用。

五、常见问题与注意事项

缓冲区空间不足：RingBuffer.Write()返回 “insufficient space” 时，需在业务中等待（如循环检查Available()）或扩容缓冲区。
重传队列堆积：若GetExpired()返回大量超时数据包，需检查网络连通性或调整重传超时参数（如InitialRTO）。
连接建立失败：确保服务端先启动，且客户端与服务端地址、端口正确，网络无防火墙拦截 UDP 协议。
并发安全：三大组件内部均通过互斥锁保证线程安全，多协程调用Send()/Receive()/Add()等接口无需额外加锁。


