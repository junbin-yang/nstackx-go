// 拥塞控制算法实现模块，提供多种经典网络拥塞控制算法的实现，用于动态调整数据发送策略，避免网络拥塞
// TODO: 后续接入fillp中使用
package congestion

import (
	"time"
)

// Controller 拥塞控制接口，定义所有拥塞控制算法需实现的核心方法
type Controller interface {
	// OnPacketSent 当数据包被发送时调用，用于更新飞行中数据包数量等状态
	OnPacketSent(packetSize int)
	// OnAckReceived 当收到ACK确认时调用，用于调整拥塞窗口、更新RTT等
	OnAckReceived(ackSize int, rtt time.Duration)
	// OnPacketLost 当检测到数据包丢失时调用，用于触发拥塞避免逻辑
	OnPacketLost()
	// GetCongestionWindow 获取当前拥塞窗口大小（字节）
	GetCongestionWindow() int
	// GetSendRate 获取当前建议的发送速率（字节/秒）
	GetSendRate() int
	// GetStatistics 获取当前拥塞控制的统计信息
	GetStatistics() CongestionStats
}

// CongestionStats 拥塞控制统计信息，用于监控和分析算法性能
type CongestionStats struct {
	CongestionWindow int           // 当前拥塞窗口大小（字节）
	Ssthresh         int           // 慢启动阈值（字节）
	RTT              time.Duration // 平均往返时间
	MinRTT           time.Duration // 最小往返时间（瓶颈链路RTT估计）
	LossRate         float64       // 丢包率（丢失包数/总发送包数）
	SendRate         int           // 实际发送速率（字节/秒）
	InFlight         int           // 飞行中数据包数量（未确认的字节数）
}

// BaseController 基础拥塞控制器，封装所有算法的通用属性和方法
type BaseController struct {
	cwnd         int           // 拥塞窗口（字节）
	ssthresh     int           // 慢启动阈值（字节）
	rtt          time.Duration // 平均往返时间
	minRTT       time.Duration // 最小往返时间（用于BBR等算法）
	inFlight     int           // 飞行中字节数（已发送未确认）
	maxCWnd      int           // 最大拥塞窗口限制
	packetSize   int           // 平均数据包大小（用于窗口计算）
	stats        CongestionStats
	sentBytes    int64         // 总发送字节数
	ackedBytes   int64         // 总确认字节数
	lostBytes    int64         // 总丢失字节数
	lastUpdate   time.Time     // 上次更新速率的时间
}

// 初始化基础控制器
func NewBaseController(initialCWnd, maxCWnd, packetSize int) *BaseController {
	return &BaseController{
		cwnd:       initialCWnd,
		ssthresh:   65536, // 默认慢启动阈值为64KB
		maxCWnd:    maxCWnd,
		packetSize: packetSize,
		lastUpdate: time.Now(),
	}
}

// 更新RTT（通用逻辑：平滑处理新RTT值）
func (b *BaseController) updateRTT(newRTT time.Duration) {
	if b.rtt == 0 {
		b.rtt = newRTT
		b.minRTT = newRTT
		return
	}
	// 平滑RTT计算（指数加权移动平均）
	b.rtt = time.Duration(0.875*float64(b.rtt) + 0.125*float64(newRTT))
	// 更新最小RTT（只保留更小的值）
	if newRTT < b.minRTT {
		b.minRTT = newRTT
	}
}

// 计算发送速率（字节/秒）
func (b *BaseController) calculateSendRate() int {
	now := time.Now()
	elapsed := now.Sub(b.lastUpdate).Seconds()
	if elapsed == 0 {
		return 0
	}
	// 基于最近确认的字节数计算速率
	rate := int(float64(b.ackedBytes) / elapsed)
	b.lastUpdate = now
	b.ackedBytes = 0 // 重置计数器
	return rate
}

// 更新统计信息
func (b *BaseController) updateStats() {
	b.stats.CongestionWindow = b.cwnd
	b.stats.Ssthresh = b.ssthresh
	b.stats.RTT = b.rtt
	b.stats.MinRTT = b.minRTT
	b.stats.InFlight = b.inFlight
	b.stats.SendRate = b.calculateSendRate()
	// 计算丢包率（总丢失数/总发送数，避免除零）
	if b.sentBytes > 0 {
		b.stats.LossRate = float64(b.lostBytes) / float64(b.sentBytes)
	}
}


// ------------------------------
// CUBIC拥塞控制算法实现（TCP CUBIC变种）
// 特点：基于时间的拥塞窗口增长，高带宽场景下性能优于传统Reno
// ------------------------------

type CubicController struct {
	*BaseController
	beta         float64       // 丢包后窗口缩减系数（通常为0.7）
	c            float64       // CUBIC系数（通常为0.4）
	lastMaxCWnd  int           // 上次拥塞时的最大窗口
	epochStart   time.Time     // 当前拥塞周期开始时间
	K            float64       // 窗口恢复到lastMaxCWnd所需的时间（秒）
}

func NewCubicController(initialCWnd, maxCWnd, packetSize int) *CubicController {
	return &CubicController{
		BaseController: NewBaseController(initialCWnd, maxCWnd, packetSize),
		beta:           0.7,
		c:              0.4,
		lastMaxCWnd:    initialCWnd,
		epochStart:     time.Now(),
	}
}

// 发送数据包时更新飞行中字节数
func (c *CubicController) OnPacketSent(packetSize int) {
	c.inFlight += packetSize
	c.sentBytes += int64(packetSize)
}

// 收到ACK时调整拥塞窗口
func (c *CubicController) OnAckReceived(ackSize int, rtt time.Duration) {
	c.inFlight -= ackSize
	c.ackedBytes += int64(ackSize)
	c.updateRTT(rtt)

	// 拥塞窗口调整逻辑
	if c.cwnd < c.ssthresh {
		// 慢启动阶段：每个ACK增加一个数据包大小
		c.cwnd += c.packetSize
	} else {
		// 拥塞避免阶段：CUBIC的核心增长公式
		elapsed := time.Since(c.epochStart).Seconds()
		// 窗口增长公式：cwnd = C*(t-K)^3 + lastMaxCWnd
		targetCWnd := int(c.c * (elapsed - c.K) * (elapsed - c.K) * (elapsed - c.K)) + c.lastMaxCWnd
		// 确保窗口不超过最大值，且每次增长不超过一个数据包大小
		if targetCWnd > c.cwnd {
			inc := targetCWnd - c.cwnd
			if inc > c.packetSize {
				inc = c.packetSize
			}
			c.cwnd += inc
		}
	}

	// 限制窗口最大值
	if c.cwnd > c.maxCWnd {
		c.cwnd = c.maxCWnd
	}
	c.updateStats()
}

// 检测到丢包时的处理
func (c *CubicController) OnPacketLost() {
	c.lostBytes += int64(c.packetSize)
	// 记录当前窗口为上次最大窗口
	c.lastMaxCWnd = c.cwnd
	// 丢包后窗口缩减为当前的beta倍
	c.cwnd = int(float64(c.cwnd) * c.beta)
	// 慢启动阈值设为缩减后的窗口
	c.ssthresh = c.cwnd
	// 重置拥塞周期
	c.epochStart = time.Now()
	// 计算K值：恢复到lastMaxCWnd所需的时间
	c.K = math.Pow(float64(c.lastMaxCWnd-c.cwnd)/c.c, 1.0/3.0)
	c.updateStats()
}

func (c *CubicController) GetCongestionWindow() int {
	return c.cwnd
}

func (c *CubicController) GetSendRate() int {
	// 发送速率 = 拥塞窗口 / RTT（字节/秒）
	if c.rtt == 0 {
		return 0
	}
	return int(float64(c.cwnd) / c.rtt.Seconds())
}

func (c *CubicController) GetStatistics() CongestionStats {
	return c.stats
}


// ------------------------------
// BBR拥塞控制算法实现（基于带宽和RTT）
// 特点：不依赖丢包检测，而是基于瓶颈链路带宽和最小RTT调整发送速率
// ------------------------------

type BBRController struct {
	*BaseController
	bwEstimate     float64       // 瓶颈带宽估计（字节/秒）
	pacingGain     float64       //  pacing速率增益系数
	cwndGain       float64       // 拥塞窗口增益系数
	state          string        // BBR状态（STARTUP/DRAIN/PROBE_BW/PROBE_RTT）
	probeRTTStart  time.Time     // PROBE_RTT状态开始时间
}

// BBR状态常量
const (
	BBRStartup   = "STARTUP"   // 启动阶段：快速提升带宽估计
	BBRDrain     = "DRAIN"     // 排水阶段：降低队列占用
	BBRProbeBW   = "PROBE_BW"  // 带宽探测阶段：稳定带宽利用
	BBRProbeRTT  = "PROBE_RTT" // RTT探测阶段：定期测量最小RTT
)

func NewBBRController(initialCWnd, maxCWnd, packetSize int) *BBRController {
	return &BBRController{
		BaseController: NewBaseController(initialCWnd, maxCWnd, packetSize),
		pacingGain:     2.0,       // 启动阶段增益
		cwndGain:       2.0,
		state:          BBRStartup,
	}
}

// 发送数据包时更新飞行中字节数
func (b *BBRController) OnPacketSent(packetSize int) {
	b.inFlight += packetSize
	b.sentBytes += int64(packetSize)
}

// 收到ACK时更新带宽估计和状态
func (b *BBRController) OnAckReceived(ackSize int, rtt time.Duration) {
	b.inFlight -= ackSize
	b.ackedBytes += int64(ackSize)
	b.updateRTT(rtt)

	// 估计瓶颈带宽（基于最近确认的字节数和RTT）
	currentBW := float64(ackSize) / rtt.Seconds()
	if currentBW > b.bwEstimate {
		b.bwEstimate = currentBW // 保留最大带宽估计
	}

	// 状态机转换逻辑
	switch b.state {
	case BBRStartup:
		// 启动阶段：如果带宽不再增长，进入排水阶段
		if currentBW < b.bwEstimate*0.9 {
			b.state = BBRDrain
			b.pacingGain = 0.5 // 降低发送速率以减少队列
		}
	case BBRDrain:
		// 排水阶段：当飞行中字节数低于带宽*RTT时，进入带宽探测
		if b.inFlight < int(b.bwEstimate*b.minRTT.Seconds()) {
			b.state = BBRProbeBW
			b.pacingGain = 1.25 // 探测更高带宽
		}
	case BBRProbeBW:
		// 定期进入RTT探测（每10秒一次）
		if time.Since(b.probeRTTStart) > 10*time.Second {
			b.state = BBRProbeRTT
			b.probeRTTStart = time.Now()
			b.cwnd = b.packetSize * 4 // 限制窗口为4个数据包
		}
	case BBRProbeRTT:
		// RTT探测持续至少200ms
		if time.Since(b.probeRTTStart) > 200*time.Millisecond {
			b.state = BBRProbeBW
			b.bwEstimate = 0 // 重置带宽估计，重新探测
		}
	}

	// 计算目标拥塞窗口：带宽 * 最小RTT * 增益系数
	targetCWnd := int(b.bwEstimate * b.minRTT.Seconds() * b.cwndGain)
	if targetCWnd > b.maxCWnd {
		targetCWnd = b.maxCWnd
	}
	b.cwnd = targetCWnd
	b.updateStats()
}

// BBR对丢包不敏感，仅轻微调整窗口
func (b *BBRController) OnPacketLost() {
	b.lostBytes += int64(b.packetSize)
	// 丢包时轻微缩减窗口（避免过度反应）
	b.cwnd = int(float64(b.cwnd) * 0.9)
	b.updateStats()
}

func (b *BBRController) GetCongestionWindow() int {
	return b.cwnd
}

func (b *BBRController) GetSendRate() int {
	// 发送速率 = 带宽估计 * pacing增益
	return int(b.bwEstimate * b.pacingGain)
}

func (b *BBRController) GetStatistics() CongestionStats {
	return b.stats
}


// ------------------------------
// Reno拥塞控制算法实现（经典TCP Reno）
// 特点：基于丢包检测，包含慢启动、拥塞避免、快速重传和快速恢复
// ------------------------------

type RenoController struct {
	*BaseController
	dupAckCount int           // 重复ACK计数器（用于检测丢包）
}

func NewRenoController(initialCWnd, maxCWnd, packetSize int) *RenoController {
	return &RenoController{
		BaseController: NewBaseController(initialCWnd, maxCWnd, packetSize),
	}
}

func (r *RenoController) OnPacketSent(packetSize int) {
	r.inFlight += packetSize
	r.sentBytes += int64(packetSize)
}

func (r *RenoController) OnAckReceived(ackSize int, rtt time.Duration) {
	r.inFlight -= ackSize
	r.ackedBytes += int64(ackSize)
	r.updateRTT(rtt)

	// 处理重复ACK（同一序列号的ACK）
	if ackSize == 0 { // 假设ackSize=0表示重复ACK
		r.dupAckCount++
		// 收到3个重复ACK，触发快速重传
		if r.dupAckCount == 3 {
			r.OnPacketLost() // 进入快速恢复
			// 快速恢复：窗口 = 慢启动阈值 + 3*MSS
			r.cwnd = r.ssthresh + 3*r.packetSize
		} else if r.dupAckCount > 3 {
			// 快速恢复阶段：每个重复ACK增加一个MSS
			r.cwnd += r.packetSize
		}
	} else {
		// 新ACK，重置重复计数器
		r.dupAckCount = 0
		// 慢启动阶段：窗口指数增长（每个ACK增加一个MSS）
		if r.cwnd < r.ssthresh {
			r.cwnd += r.packetSize
		} else {
			// 拥塞避免阶段：窗口线性增长（每收到一个窗口的ACK增加一个MSS）
			r.cwnd += r.packetSize * r.packetSize / r.cwnd
		}
	}

	if r.cwnd > r.maxCWnd {
		r.cwnd = r.maxCWnd
	}
	r.updateStats()
}

func (r *RenoController) OnPacketLost() {
	r.lostBytes += int64(r.packetSize)
	// 丢包后：慢启动阈值 = 当前窗口/2，窗口重置为慢启动阈值（或最小值）
	r.ssthresh = r.cwnd / 2
	if r.ssthresh < 2*r.packetSize { // 确保阈值不小于2个MSS
		r.ssthresh = 2 * r.packetSize
	}
	r.cwnd = r.ssthresh // 进入拥塞避免阶段
	r.updateStats()
}

func (r *RenoController) GetCongestionWindow() int {
	return r.cwnd
}

func (r *RenoController) GetSendRate() int {
	if r.rtt == 0 {
		return 0
	}
	return int(float64(r.cwnd) / r.rtt.Seconds())
}

func (r *RenoController) GetStatistics() CongestionStats {
	return r.stats
}


// ------------------------------
// Vegas拥塞控制算法实现（基于延迟的拥塞控制）
// 特点：通过比较预期吞吐量和实际吞吐量检测拥塞，避免等到丢包才反应
// ------------------------------

type VegasController struct {
	*BaseController
	alpha         int           // 最小允许的吞吐量差异（字节）
	beta          int           // 最大允许的吞吐量差异（字节）
	expectedRate  float64       // 预期吞吐量（基于最小RTT）
}

func NewVegasController(initialCWnd, maxCWnd, packetSize int) *VegasController {
	return &VegasController{
		BaseController: NewBaseController(initialCWnd, maxCWnd, packetSize),
		alpha:          3*packetSize, // 通常为3个MSS
		beta:           6*packetSize,  // 通常为6个MSS
	}
}

func (v *VegasController) OnPacketSent(packetSize int) {
	v.inFlight += packetSize
	v.sentBytes += int64(packetSize)
}

func (v *VegasController) OnAckReceived(ackSize int, rtt time.Duration) {
	v.inFlight -= ackSize
	v.ackedBytes += int64(ackSize)
	v.updateRTT(rtt)

	// 计算预期吞吐量（基于最小RTT）和实际吞吐量
	actualRate := float64(ackSize) / rtt.Seconds()
	v.expectedRate = float64(v.cwnd) / v.minRTT.Seconds() // 理想无拥塞时的速率
	throughputDiff := v.expectedRate - actualRate        // 吞吐量差异（越大说明拥塞越严重）

	// 根据吞吐量差异调整窗口
	if throughputDiff < float64(v.alpha) {
		// 无拥塞：增加窗口
		v.cwnd += v.packetSize
	} else if throughputDiff > float64(v.beta) {
		// 严重拥塞：减少窗口
		v.cwnd -= v.packetSize
	}
	// 介于alpha和beta之间：保持窗口不变

	if v.cwnd > v.maxCWnd {
		v.cwnd = v.maxCWnd
	}
	if v.cwnd < v.packetSize { // 窗口不小于1个MSS
		v.cwnd = v.packetSize
	}
	v.updateStats()
}

// Vegas对丢包的处理更保守（丢包通常意味着严重拥塞）
func (v *VegasController) OnPacketLost() {
	v.lostBytes += int64(v.packetSize)
	v.ssthresh = v.cwnd / 2 // 阈值减半
	v.cwnd = v.ssthresh     // 窗口重置为阈值
	v.updateStats()
}

func (v *VegasController) GetCongestionWindow() int {
	return v.cwnd
}

func (v *VegasController) GetSendRate() int {
	return int(v.expectedRate) // 基于预期速率发送
}

func (v *VegasController) GetStatistics() CongestionStats {
	return v.stats
}


// 创建拥塞控制器实例（根据算法类型）
func NewController(algorithm string, initialCWnd, maxCWnd, packetSize int) (Controller, error) {
	switch algorithm {
	case "cubic":
		return NewCubicController(initialCWnd, maxCWnd, packetSize), nil
	case "bbr":
		return NewBBRController(initialCWnd, maxCWnd, packetSize), nil
	case "reno":
		return NewRenoController(initialCWnd, maxCWnd, packetSize), nil
	case "vegas":
		return NewVegasController(initialCWnd, maxCWnd, packetSize), nil
	default:
		return nil, fmt.Errorf("不支持的拥塞控制算法: %s", algorithm)
	}
}

