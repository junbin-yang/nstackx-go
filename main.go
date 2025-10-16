// NStackX-Go的命令行接口，用于启动分布式设备发现服务及执行相关操作
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/junbin-yang/nstackx-go/api"
	"github.com/junbin-yang/nstackx-go/pkg/discovery/service"
	log "github.com/junbin-yang/nstackx-go/pkg/utils/logger"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var (
	// 版本信息（编译时可通过参数注入）
	Version   = "dev"     // 版本号
	BuildTime = "unknown" // 构建时间

	// 配置相关
	cfgFile string     // 配置文件路径
	config  api.Config // 服务配置结构体

	// 日志实例
	logger *log.Logger
)

// rootCmd 表示基础命令（默认命令）
var rootCmd = &cobra.Command{
	Use:   "nstackx",
	Short: "NStackX-Go: 分布式设备发现服务",
	Long: `NStackX-Go是高性能、跨平台的鸿蒙分布式设备发现服务的Go实现。
它使用CoAP协议在本地网络中提供自动设备发现，支持主动和被动两种发现模式。`,
	Run: runServer, // 执行root命令时调用runServer函数
}

// versionCmd 表示版本命令（用于显示版本信息）
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "打印版本信息",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("NStackX-Go %s\n", Version)
		fmt.Printf("构建时间: %s\n", BuildTime)
	},
}

// discoverCmd 表示发现命令（用于手动触发设备发现）
var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "启动设备发现",
	Run:   runDiscovery, // 执行discover命令时调用runDiscovery函数
}

func init() {
	// 在命令执行前初始化配置
	cobra.OnInitialize(initConfig)

	// 全局标志（所有命令共享）
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "配置文件路径（默认是./nstackx.yaml）")
	rootCmd.PersistentFlags().String("log-level", "info", "日志级别（debug, info, warning, error, fatal）")
	rootCmd.PersistentFlags().String("device-id", "", "本地设备ID")
	rootCmd.PersistentFlags().String("device-name", "", "本地设备名称")
	rootCmd.PersistentFlags().Uint32("device-type", 0, "本地设备类型")
	rootCmd.PersistentFlags().Uint32("max-devices", 20, "最大跟踪设备数量")
	rootCmd.PersistentFlags().Duration("aging-time", 60, "设备老化时间（秒）")
	rootCmd.PersistentFlags().Bool("enable-stats", false, "启用统计信息收集")

	// 将命令行标志绑定到viper（用于配置读取）
	viper.BindPFlag("log-level", rootCmd.PersistentFlags().Lookup("log-level"))
	viper.BindPFlag("device-id", rootCmd.PersistentFlags().Lookup("device-id"))
	viper.BindPFlag("device-name", rootCmd.PersistentFlags().Lookup("device-name"))
	viper.BindPFlag("device-type", rootCmd.PersistentFlags().Lookup("device-type"))
	viper.BindPFlag("max-devices", rootCmd.PersistentFlags().Lookup("max-devices"))
	viper.BindPFlag("aging-time", rootCmd.PersistentFlags().Lookup("aging-time"))
	viper.BindPFlag("enable-stats", rootCmd.PersistentFlags().Lookup("enable-stats"))

	// 发现命令专属标志
	discoverCmd.Flags().String("mode", "active", "发现模式（active=主动, passive=被动）")
	discoverCmd.Flags().Uint32("count", 10, "发现广播次数")
	discoverCmd.Flags().Duration("duration", 30, "发现持续时间（秒）")
	discoverCmd.Flags().Duration("interval", 1, "发现间隔（秒）")

	// 添加子命令到根命令
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(discoverCmd)
}

// initConfig 初始化配置：读取配置文件、设置默认值、初始化日志
func initConfig() {
	// 配置文件处理
	if cfgFile != "" {
		// 若指定了配置文件路径，直接使用
		viper.SetConfigFile(cfgFile)
	} else {
		// 未指定则在当前目录查找nstackx.yaml
		viper.AddConfigPath(".")
		viper.SetConfigName("nstackx")
		viper.SetConfigType("yaml")
	}

	// 环境变量前缀为NSTACKX（例如NSTACKX_LOG_LEVEL对应log-level）
	viper.SetEnvPrefix("NSTACKX")
	viper.AutomaticEnv() // 自动读取环境变量

	// 读取配置文件（若存在）
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("使用配置文件:", viper.ConfigFileUsed())
	}

	// 初始化日志（开发环境配置）
	logger = log.Default()
}

// runServer 执行root命令：启动NStackX服务，运行设备发现并处理中断信号
func runServer(cmd *cobra.Command, args []string) {
	// 加载配置到config结构体
	loadConfig()

	logger.Info("启动NStackX-Go服务",
		zap.String("版本", Version),
		zap.String("设备ID", config.LocalDevice.DeviceID),
		zap.String("设备名称", config.LocalDevice.Name))

	// 创建设备发现服务实例
	svc, err := service.NewDiscoveryService(&config)
	if err != nil {
		logger.Fatal("创建发现服务失败", zap.Error(err))
	}

	// 注册回调函数（处理设备发现、丢失和错误事件）
	svc.RegisterCallbacks(&api.Callbacks{
		OnDeviceFound: func(device *api.DeviceInfo) {
			logger.Info("发现新设备",
				zap.String("设备ID", device.DeviceID),
				zap.String("设备名称", device.DeviceName),
				zap.String("IP地址", device.NetworkIP.String()))
		},
		OnDeviceLost: func(deviceID string) {
			logger.Info("设备离线", zap.String("设备ID", deviceID))
		},
		OnError: func(err error) {
			logger.Error("服务错误", zap.Error(err))
		},
	})

	// 启动发现服务
	if err := svc.Start(); err != nil {
		logger.Fatal("启动发现服务失败", zap.Error(err))
	}

	// 配置发现参数
	settings := &api.DiscoverySettings{
		DiscoveryMode:     api.ModeActive,   // 主动发现模式
		AdvertiseCount:    10,               // 广播次数
		AdvertiseDuration: 30 * time.Second, // 总持续时间
		AdvertiseInterval: 1 * time.Second,  // 广播间隔
	}

	// 启动设备发现
	if err := svc.StartDiscovery(settings); err != nil {
		logger.Error("启动设备发现失败", zap.Error(err))
	}

	// 等待中断信号（优雅关闭）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 监听系统中断信号（SIGINT=Ctrl+C, SIGTERM=终止信号）
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 阻塞等待中断信号或上下文取消
	select {
	case sig := <-sigChan:
		logger.Info("收到中断信号，开始关闭服务", zap.String("信号", sig.String()))
	case <-ctx.Done():
		logger.Info("上下文已取消，开始关闭服务")
	}

	// 停止服务
	if err := svc.Stop(); err != nil {
		logger.Error("停止服务失败", zap.Error(err))
	}

	// 销毁服务资源
	svc.Destroy()
	logger.Info("NStackX-Go服务已停止")
}

// runDiscovery 执行discover命令：启动单次设备发现，输出结果后退出
func runDiscovery(cmd *cobra.Command, args []string) {
	// 加载配置
	loadConfig()

	// 读取命令行参数（发现模式、次数、持续时间、间隔）
	mode, _ := cmd.Flags().GetString("mode")
	count, _ := cmd.Flags().GetUint32("count")
	duration, _ := cmd.Flags().GetDuration("duration")
	interval, _ := cmd.Flags().GetDuration("interval")

	logger.Info("启动设备发现",
		zap.String("模式", mode),
		zap.Uint32("广播次数", count),
		zap.Duration("持续时间", duration),
		zap.Duration("间隔", interval))

	// 创建发现服务实例
	svc, err := service.NewDiscoveryService(&config)
	if err != nil {
		logger.Fatal("创建发现服务失败", zap.Error(err))
	}

	// 注册回调（统计并打印发现的设备）
	deviceCount := 0
	svc.RegisterCallbacks(&api.Callbacks{
		OnDeviceFound: func(device *api.DeviceInfo) {
			deviceCount++
			fmt.Printf("[%d] 发现设备: %s (%s) 地址: %s\n",
				deviceCount, device.DeviceName, device.DeviceID, device.NetworkIP)
		},
	})

	// 启动服务
	if err := svc.Start(); err != nil {
		logger.Fatal("启动发现服务失败", zap.Error(err))
	}

	// 解析发现模式（字符串转枚举）
	var discoveryMode api.DiscoveryMode
	switch mode {
	case "active":
		discoveryMode = api.ModeActive // 主动模式：发送广播请求
	case "passive":
		discoveryMode = api.ModePassive // 被动模式：等待并响应请求
	default:
		discoveryMode = api.ModeActive // 默认主动模式
	}

	// 配置发现参数
	settings := &api.DiscoverySettings{
		DiscoveryMode:     discoveryMode,
		AdvertiseCount:    count,
		AdvertiseDuration: duration,
		AdvertiseInterval: interval,
	}

	// 启动设备发现
	if err := svc.StartDiscovery(settings); err != nil {
		logger.Fatal("启动设备发现失败", zap.Error(err))
	}

	// 等待发现持续时间（阻塞等待）
	time.Sleep(duration)

	// 获取最终设备列表并打印
	devices, err := svc.GetDeviceList()
	if err != nil {
		logger.Error("获取设备列表失败", zap.Error(err))
	} else {
		fmt.Printf("\n发现完成。共发现 %d 台设备:\n", len(devices))
		for i, device := range devices {
			fmt.Printf("%d. %s (%s) - 类型: %d, IP: %s\n",
				i+1, device.DeviceName, device.DeviceID, device.DeviceType, device.NetworkIP)
		}
	}

	// 停止并销毁服务
	svc.Stop()
	svc.Destroy()
}

// loadConfig 从viper加载配置到config结构体，补全默认值
func loadConfig() {
	// 从viper读取配置（viper已绑定命令行标志和配置文件）
	config = api.Config{
		LocalDevice: api.LocalDeviceInfo{
			DeviceID:   viper.GetString("device-id"),                   // 设备ID
			Name:       viper.GetString("device-name"),                 // 设备名称
			DeviceType: api.DeviceType(viper.GetUint32("device-type")), // 设备类型
		},
		MaxDeviceNum:     viper.GetUint32("max-devices"),  // 最大设备数
		AgingTime:        viper.GetDuration("aging-time"), // 设备老化时间
		LogLevel:         viper.GetString("log-level"),    // 日志级别
		EnableStatistics: viper.GetBool("enable-stats"),   // 是否启用统计
	}

	// 若未指定设备ID，自动生成（基于主机名和进程ID）
	if config.LocalDevice.DeviceID == "" {
		hostname, _ := os.Hostname()
		config.LocalDevice.DeviceID = fmt.Sprintf("nstackx-go-%s-%d", hostname, os.Getpid())
	}

	// 若未指定设备名称，使用默认名称
	if config.LocalDevice.Name == "" {
		config.LocalDevice.Name = "NStackX-Go 设备"
	}
}

// main 函数：执行root命令
func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

// ./nstackx --device-id "device-A" --device-name "Device A" --log-level debug

// ./nstackx discover --mode active --duration 10s --interval 2s --log-level debug
