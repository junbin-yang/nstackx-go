package main

import (
        "fmt"
        "net"
        "time"
        "github.com/junbin-yang/nstackx-go/pkg/transport/fillp"
)

// 客户端示例（发送数据）
func main() {
	// 1. 定义客户端本地地址（可选，为空则随机端口）和服务端远程地址（必须）
	localAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:8081") // 可选
	remoteAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:8080")
	if err != nil {
		fmt.Printf("解析远程地址失败：%v\n", err)
		return
	}

	// 2. 初始化连接（传入本地和远程地址）
	conn, err := fillp.NewConnection(localAddr, remoteAddr)
	if err != nil {
		fmt.Printf("初始化客户端连接失败：%v\n", err)
		return
	}
	defer conn.Close()

	// 3. 发起连接（无需传地址，使用 NewConnection 中的 remoteAddr）
	fmt.Println("客户端尝试连接...")
	if err := conn.Connect(); err != nil {
		fmt.Printf("连接失败：%v\n", err)
		return
	}
	fmt.Printf("客户端已连接（本地：%s，远程：%s）\n", 
		conn.LocalAddr(), conn.RemoteAddr())

	// 4. 发送测试数据
	testData := []string{"Hello FILLP", "This is a test", "Bye!"}
	for _, data := range testData {
		if err := conn.Send([]byte(data)); err != nil {
			fmt.Printf("发送失败：%v\n", err)
			return
		}
		fmt.Printf("发送数据：%s\n", data)

		// 接收响应
		resp, err := conn.ReceiveWithTimeout(3 * time.Second)
		if err != nil {
			fmt.Printf("接收响应失败：%v\n", err)
			return
		}
		fmt.Printf("收到响应：%s\n", string(resp))

		time.Sleep(1 * time.Second)
	}
}
