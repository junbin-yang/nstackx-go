package network

import (
	"fmt"
	"net"
	"testing"
)

func Test_NetworkManager(t *testing.T) {
	fmt.Println("[NetworkManager]")

	// 1. 初始化网络管理器
	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("网络管理器初始化失败: %v", err)
	}
	defer mgr.Stop()
	fmt.Println("网络管理器初始化成功")

	// 2. 获取所有网络接口并显示全部接口
	ifaces := mgr.GetInterfaces()
	if len(ifaces) == 0 {
		t.Error("未发现任何网络接口")
		return
	}
	fmt.Printf("发现 %d 个网络接口\n", len(ifaces))
	// 显示所有接口（不再限制前3个）
	for i, iface := range ifaces {
		fmt.Printf("  接口 %d: %s (索引: %d, MAC: %s)\n", 
			i+1, iface.Name, iface.Index, iface.MAC)
	}

	// 3. 获取第一个接口的详细信息（如果存在）
	if len(ifaces) > 0 {
		testIfaceName := ifaces[0].Name
		iface, err := mgr.GetInterface(testIfaceName)
		if err != nil {
			t.Errorf("获取接口 %s 信息失败: %v", testIfaceName, err)
		} else {
			fmt.Printf("接口 %s 详细信息: 状态=%v, IP地址数=%d\n", 
				iface.Name, iface.Flags&net.FlagUp != 0, len(iface.Addresses))
		}
	} else {
		t.Skip("没有可用接口，跳过后续依赖接口的测试步骤")
	}

	// 4. 启动网络监控
	err = mgr.Start()
	if err != nil {
		t.Errorf("启动网络监控失败: %v", err)
		return
	}
	fmt.Println("网络监控启动成功")

	// 5. 获取活跃接口
	activeIfaces := mgr.GetActiveInterfaces()
	fmt.Printf("发现 %d 个活跃接口\n", len(activeIfaces))
	// 显示所有活跃接口
	for i, iface := range activeIfaces {
		fmt.Printf("  活跃接口 %d: %s\n", i+1, iface.Name)
	}

	// 6. 获取多播接口
	multicastIfaces := mgr.GetMulticastInterfaces()
	fmt.Printf("发现 %d 个支持多播的接口\n", len(multicastIfaces))
	// 显示所有多播接口
	for i, iface := range multicastIfaces {
		fmt.Printf("  多播接口 %d: %s\n", i+1, iface.Name)
	}

	// 7. 获取默认接口
	defaultIface, err := mgr.GetDefaultInterface()
	if err != nil {
		fmt.Printf("获取默认接口失败: %v\n", err)
	} else {
		fmt.Printf("默认接口: %s\n", defaultIface.Name)
	}

	// 8. 检查第一个接口的状态（如果存在）
	if len(ifaces) > 0 {
		testIfaceName := ifaces[0].Name
		isUp := mgr.IsInterfaceUp(testIfaceName)
		fmt.Printf("接口 %s 状态: %s\n", testIfaceName, map[bool]string{true: "UP", false: "DOWN"}[isUp])
	}

	// 9. 获取并显示所有IP地址信息
	ipv4Addrs := mgr.GetIPv4Addresses()
	ipv6Addrs := mgr.GetIPv6Addresses()
	fmt.Printf("发现 %d 个IPv4地址, %d 个IPv6地址\n", len(ipv4Addrs), len(ipv6Addrs))
	
	// 显示所有IPv4地址
	for i, addr := range ipv4Addrs {
		fmt.Printf("  IPv4 %d: %s\n", i+1, addr)
	}
	// 显示所有IPv6地址
	for i, addr := range ipv6Addrs {
		fmt.Printf("  IPv6 %d: %s\n", i+1, addr)
	}

	// 10. 停止网络监控
	mgr.Stop()
	fmt.Println("网络监控已停止")

	// 11. 验证监控状态
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	fmt.Printf("监控最终状态: %v\n", mgr.monitoring)
}

