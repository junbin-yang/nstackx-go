package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/junbin-yang/nstackx-go/pkg/transport/fillp"
)

// 简单判定是否为“正常关闭”类错误
func isConnClosedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "use of closed network connection")
}

// 服务端示例（接收数据，按会话循环）
func main() {
	fmt.Println("服务端启动监听...")

	// 外层循环：每个客户端会话一个 Connection，会话结束后继续等待下一次连接
	for {
		// 1) 构造本地地址与新会话的 Connection
		localAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:8080")
		if err != nil {
			fmt.Printf("解析本地地址失败：%v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		conn, err := fillp.NewConnection(localAddr, nil)
		if err != nil {
			fmt.Printf("初始化服务端连接失败：%v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}

		// 2) 监听并等待客户端连接
		if err := conn.Listen(); err != nil {
			fmt.Printf("服务端监听失败：%v\n", err)
			// 监听失败，关闭本次会话对象后重试
			_ = conn.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		fmt.Printf("服务端已连接（本地：%s，远程：%s）\n", conn.LocalAddr(), conn.RemoteAddr())

		// 3) 会话循环：接收请求并回响应
		for {
			data, err := conn.Receive()
			if err != nil {
				// 正常关闭：结束当前会话，返回外层循环继续等待新连接
				if isConnClosedErr(err) {
					fmt.Printf("会话结束：%v\n", err)
				} else {
					fmt.Printf("接收数据失败：%v\n", err)
				}
				break
			}
			fmt.Printf("收到数据：%s（长度：%d）\n", string(data), len(data))

			resp := []byte(fmt.Sprintf("服务端已收到：%s", string(data)))
			if err := conn.Send(resp); err != nil {
				fmt.Printf("发送响应失败：%v\n", err)
				break
			}

			// 演示节奏
			time.Sleep(200 * time.Millisecond)
		}

		// 4) 关闭本次会话连接，进入下一轮
		conn.Close()
		fmt.Println("等待下一位客户端连接...")
	}
}
