// 提供CoAP消息的编码与解码功能
package coap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// CoAP消息格式相关常量
const (
	// Version CoAP协议版本（仅支持v1，符合RFC 7252标准）
	Version1 = 1

	// Message types CoAP消息类型（4种核心类型）
	TypeConfirmable    = 0 // 确认型消息（CON）：需接收方回复ACK确认
	TypeNonConfirmable = 1 // 非确认型消息（NON）：无需回复，适用于低延迟场景
	TypeAcknowledgment = 2 // 确认消息（ACK）：用于回复CON类型消息
	TypeReset          = 3 // 重置消息（RST）：用于告知发送方消息处理失败

	// Option numbers CoAP标准选项号（对应RFC 7252定义）
	OptionIfMatch       = 1  // If-Match：用于条件请求（匹配资源ETag）
	OptionUriHost       = 3  // Uri-Host：资源主机地址（如域名或IP）
	OptionETag          = 4  // ETag：资源的实体标签（用于缓存和并发控制）
	OptionIfNoneMatch   = 5  // If-None-Match：条件请求（仅当资源不存在时处理）
	OptionUriPort       = 7  // Uri-Port：资源端口号（默认5683）
	OptionLocationPath  = 8  // Location-Path：资源创建后的位置路径
	OptionUriPath       = 11 // Uri-Path：资源路径（如"/discover"，类似HTTP的URL路径）
	OptionContentFormat = 12 // Content-Format：负载数据格式（如JSON、CBOR）
	OptionMaxAge        = 14 // Max-Age：资源缓存最大有效时间（单位：秒）
	OptionUriQuery      = 15 // Uri-Query：请求参数（如"?id=123"）
	OptionAccept        = 17 // Accept：接收方期望的负载格式（与Content-Format对应）
	OptionLocationQuery = 20 // Location-Query：资源创建后的位置参数
	OptionBlock2        = 23 // Block2：用于大负载的分块传输（针对响应数据）
	OptionBlock1        = 27 // Block1：用于大负载的分块传输（针对请求数据）
	OptionSize2         = 28 // Size2：响应负载的预估大小
	OptionProxyUri      = 35 // Proxy-Uri：代理服务器的目标URI
	OptionProxyScheme   = 39 // Proxy-Scheme：代理服务器的协议方案（如coap、coaps）
	OptionSize1         = 60 // Size1：请求负载的预估大小

	// Content formats 负载数据格式编码（对应RFC 7252及扩展定义）
	ContentFormatText        = 0  // 文本格式（text/plain）
	ContentFormatLinkFormat  = 40 // 链接格式（application/link-format）
	ContentFormatXML         = 41 // XML格式（application/xml）
	ContentFormatOctetStream = 42 // 二进制流（application/octet-stream）
	ContentFormatExi         = 47 // EXI格式（高效XML交换格式）
	ContentFormatJSON        = 50 // JSON格式（application/json）
	ContentFormatCBOR        = 60 // CBOR格式（紧凑二进制对象表示）

	// Payload marker 负载分隔符（固定为0xFF）：用于区分选项与负载的边界
	PayloadMarker = 0xFF
)

// Option 表示一个CoAP选项（选项号+选项值的组合）
type Option struct {
	Number uint16 // 选项号（对应上方OptionXXX常量）
	Value  []byte // 选项值（根据选项类型存储对应数据，如Uri-Path的路径字节流）
}

// RawMessage 表示原始的CoAP消息结构（完整的CoAP消息组成部分）
type RawMessage struct {
	Version     uint8    // 协议版本（固定为Version1=1）
	Type        uint8    // 消息类型（对应TypeXXX常量）
	TokenLength uint8    // 令牌（Token）长度（0-8字节，CoAP协议限制）
	Code        uint8    // 消息码（请求时为方法码如GET/POST，响应时为状态码如2.05）
	MessageID   uint16   // 消息ID（2字节，用于关联请求与响应，避免消息混淆）
	Token       []byte   // 令牌（可选，用于上下文关联，长度由TokenLength指定）
	Options     []Option // 选项列表（消息的附加属性，如Uri-Path、Content-Format）
	Payload     []byte   // 消息负载（业务数据，如设备发现的JSON数据）
}

// Encoder 处理CoAP消息的编码（结构体→字节流）与解码（字节流→结构体）
type Encoder struct{}

// NewEncoder 创建一个新的CoAP编解码器实例
func NewEncoder() *Encoder {
	return &Encoder{}
}

// Encode 将CoAP消息结构体（RawMessage）编码为网络传输用的字节流
// 参数：msg - 待编码的CoAP消息结构体
// 返回：编码后的字节流，若出错则返回错误信息
func (e *Encoder) Encode(msg *RawMessage) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("消息不能为空")
	}

	// 验证消息合法性（避免无效格式，如版本错误、令牌长度超限）
	if err := e.validateMessage(msg); err != nil {
		return nil, err
	}

	// 用缓冲区拼接编码后的内容
	buf := new(bytes.Buffer)

	// 1. 编码消息头部（4字节，CoAP消息的固定起始部分）
	header := e.encodeHeader(msg)
	if err := binary.Write(buf, binary.BigEndian, header); err != nil {
		return nil, fmt.Errorf("写入头部失败: %w", err)
	}

	// 2. 编码令牌（Token）（若长度大于0则写入）
	if msg.TokenLength > 0 && len(msg.Token) > 0 {
		// 仅写入TokenLength指定长度的字节（避免Token实际长度超出声明）
		if _, err := buf.Write(msg.Token[:msg.TokenLength]); err != nil {
			return nil, fmt.Errorf("写入令牌失败: %w", err)
		}
	}

	// 3. 编码选项（Options）（若存在选项则编码写入）
	if len(msg.Options) > 0 {
		optionBytes, err := e.encodeOptions(msg.Options)
		if err != nil {
			return nil, fmt.Errorf("编码选项失败: %w", err)
		}
		if _, err := buf.Write(optionBytes); err != nil {
			return nil, fmt.Errorf("写入选项失败: %w", err)
		}
	}

	// 4. 编码负载（Payload）（若存在负载则写入，需先加负载分隔符）
	if len(msg.Payload) > 0 {
		// 先写入负载分隔符（0xFF），标记选项结束、负载开始
		if err := buf.WriteByte(PayloadMarker); err != nil {
			return nil, fmt.Errorf("写入负载分隔符失败: %w", err)
		}
		// 写入实际负载数据
		if _, err := buf.Write(msg.Payload); err != nil {
			return nil, fmt.Errorf("写入负载失败: %w", err)
		}
	}

	// 返回完整的编码字节流
	return buf.Bytes(), nil
}

// Decode 将网络接收的字节流解码为CoAP消息结构体（RawMessage）
// 参数：data - 待解码的字节流
// 返回：解码后的CoAP消息结构体，若出错则返回错误信息
func (e *Encoder) Decode(data []byte) (*RawMessage, error) {
	// CoAP消息头部至少4字节，若不足则为无效消息
	if len(data) < 4 {
		return nil, fmt.Errorf("消息过短: 仅%d字节（至少需4字节头部）", len(data))
	}

	// 初始化空的消息结构体
	msg := &RawMessage{}
	// 用字节流读取器处理数据
	reader := bytes.NewReader(data)

	// 1. 解码消息头部（4字节，按大端序读取）
	var header uint32
	if err := binary.Read(reader, binary.BigEndian, &header); err != nil {
		return nil, fmt.Errorf("读取头部失败: %w", err)
	}
	// 解析头部字段到消息结构体
	e.decodeHeader(header, msg)

	// 验证协议版本（仅支持v1）
	if msg.Version != Version1 {
		return nil, fmt.Errorf("不支持的协议版本: %d（仅支持CoAP v1）", msg.Version)
	}

	// 2. 解码令牌（Token）（若长度大于0则读取）
	if msg.TokenLength > 0 {
		// 按TokenLength分配缓冲区
		msg.Token = make([]byte, msg.TokenLength)
		if _, err := reader.Read(msg.Token); err != nil {
			return nil, fmt.Errorf("读取令牌失败: %w", err)
		}
	}

	// 3. 解码选项（Options）与负载（Payload）（读取剩余所有数据）
	var remaining []byte
	if reader.Len() > 0 {
    		remaining = make([]byte, reader.Len())
    		if _, err := reader.Read(remaining); err != nil {
        		return nil, fmt.Errorf("读取剩余数据（选项+负载）失败: %w", err)
    		}
	} else {
    		remaining = []byte{} // 无剩余数据时直接赋空切片
	}

	// 查找负载分隔符（0xFF），区分选项与负载
	payloadIndex := bytes.IndexByte(remaining, PayloadMarker)

	var optionData []byte // 存储选项对应的字节流
	if payloadIndex >= 0 {
		// 分隔符前为选项数据，分隔符后为负载数据
		optionData = remaining[:payloadIndex]
		msg.Payload = remaining[payloadIndex+1:]
	} else {
		// 无负载分隔符，所有剩余数据均为选项（无负载）
		optionData = remaining
	}

	// 解码选项数据（若存在选项则解析）
	if len(optionData) > 0 {
		options, err := e.decodeOptions(optionData)
		if err != nil {
			return nil, fmt.Errorf("解码选项失败: %w", err)
		}
		msg.Options = options
	}

	// 返回解码后的消息结构体
	return msg, nil
}

// encodeHeader 将RawMessage的头部字段编码为4字节的uint32（大端序）
// 参数：msg - 待编码的CoAP消息结构体
// 返回：编码后的4字节头部（uint32类型，需后续按大端序写入字节流）
func (e *Encoder) encodeHeader(msg *RawMessage) uint32 {
	var header uint32

	// 版本（2位）：取Version的低2位，左移30位（头部第1字节的高2位）
	header |= uint32(msg.Version&0x03) << 30

	// 消息类型（2位）：取Type的低2位，左移28位（头部第1字节的中2位）
	header |= uint32(msg.Type&0x03) << 28

	// 令牌长度（4位）：取TokenLength的低4位，左移24位（头部第1字节的低4位）
	header |= uint32(msg.TokenLength&0x0F) << 24

	// 消息码（8位）：取Code的低8位，左移16位（头部第2字节）
	header |= uint32(msg.Code) << 16

	// 消息ID（16位）：取MessageID的低16位，占头部第3-4字节
	header |= uint32(msg.MessageID)

	return header
}

// decodeHeader 将4字节的头部（uint32）解析为RawMessage的头部字段
// 参数：header - 待解析的4字节头部（uint32类型）
// 参数：msg - 解析后字段的存储目标（RawMessage结构体）
func (e *Encoder) decodeHeader(header uint32, msg *RawMessage) {
	// 版本：右移30位，取低2位
	msg.Version = uint8((header >> 30) & 0x03)
	// 消息类型：右移28位，取低2位
	msg.Type = uint8((header >> 28) & 0x03)
	// 令牌长度：右移24位，取低4位
	msg.TokenLength = uint8((header >> 24) & 0x0F)
	// 消息码：右移16位，取低8位
	msg.Code = uint8((header >> 16) & 0xFF)
	// 消息ID：取低16位
	msg.MessageID = uint16(header & 0xFFFF)
}

// encodeOptions 对选项列表进行编码（按CoAP标准的delta编码规则）
// 步骤：1. 按选项号排序 2. 计算delta（当前与前一选项号的差值）3. 编码delta、长度及选项值
// 参数：options - 待编码的选项列表
// 返回：编码后的选项字节流，若出错则返回错误信息
func (e *Encoder) encodeOptions(options []Option) ([]byte, error) {
	// 1. 按选项号升序排序（CoAP协议要求选项必须按编号递增排列）
	sort.Slice(options, func(i, j int) bool {
		return options[i].Number < options[j].Number
	})

	buf := new(bytes.Buffer)
	prevNumber := uint16(0) // 前一个选项的编号（初始为0）

	// 遍历每个选项进行编码
	for _, opt := range options {
		// 计算delta：当前选项号与前一选项号的差值（用于压缩编码）
		delta := opt.Number - prevNumber
		// 选项值的长度
		length := uint16(len(opt.Value))

		// 2. 编码选项头部（delta+长度），返回头部字节、扩展delta（可选）、扩展长度（可选）
		optHeader, extDelta, extLength := e.encodeOptionHeader(delta, length)

		// 写入选项头部字节
		if err := buf.WriteByte(optHeader); err != nil {
			return nil, err
		}

		// 写入扩展delta（若delta超过12，需额外字节存储）
		if extDelta != nil {
			if _, err := buf.Write(extDelta); err != nil {
				return nil, err
			}
		}

		// 写入扩展长度（若长度超过12，需额外字节存储）
		if extLength != nil {
			if _, err := buf.Write(extLength); err != nil {
				return nil, err
			}
		}

		// 3. 写入选项值（若选项值非空）
		if len(opt.Value) > 0 {
			if _, err := buf.Write(opt.Value); err != nil {
				return nil, err
			}
		}

		// 更新前一选项号，为下一个选项计算delta做准备
		prevNumber = opt.Number
	}

	return buf.Bytes(), nil
}

// decodeOptions 对选项字节流进行解码（按CoAP标准的delta编码规则）
// 步骤：1. 解析选项头部（delta+长度）2. 计算实际选项号 3. 读取选项值
// 参数：data - 待解码的选项字节流
// 返回：解码后的选项列表，若出错则返回错误信息
func (e *Encoder) decodeOptions(data []byte) ([]Option, error) {
	options := make([]Option, 0) // 存储解码后的选项
	reader := bytes.NewReader(data)
	prevNumber := uint16(0) // 前一个选项的编号（初始为0）

	// 循环读取所有选项（直到字节流读完）
	for reader.Len() > 0 {
		// 1. 读取选项头部（1字节，包含delta的高4位和长度的低4位）
		optHeader, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("读取选项头部失败: %w", err)
		}

		// 检查是否误读了负载分隔符（理论上选项字节流中不应包含0xFF）
		if optHeader == PayloadMarker {
			break
		}

		// 解析delta（高4位）和长度（低4位）
		delta := uint16((optHeader >> 4) & 0x0F)
		length := uint16(optHeader & 0x0F)

		// 读取扩展delta（若delta为13/14，需额外字节；15为保留值，不支持）
		delta, err = e.readExtendedValue(reader, delta)
		if err != nil {
			return nil, fmt.Errorf("读取扩展delta失败: %w", err)
		}

		// 读取扩展长度（同理，长度为13/14时需额外字节）
		length, err = e.readExtendedValue(reader, length)
		if err != nil {
			return nil, fmt.Errorf("读取扩展长度失败: %w", err)
		}

		// 2. 计算当前选项号（前一选项号 + delta）
		optNumber := prevNumber + delta

		// 3. 读取选项值（按解析出的长度分配缓冲区）
		value := make([]byte, length)
		if length > 0 {
			if _, err := reader.Read(value); err != nil {
				return nil, fmt.Errorf("读取选项值失败: %w", err)
			}
		}

		// 将解码后的选项添加到列表
		options = append(options, Option{
			Number: optNumber,
			Value:  value,
		})

		// 更新前一选项号，为下一个选项解码做准备
		prevNumber = optNumber
	}

	return options, nil
}

// encodeOptionHeader 编码选项头部（delta+长度），处理delta和长度的扩展逻辑
// 规则（CoAP标准）：
// - delta/长度 <13：直接用4位存储（无需扩展）
// - 13 ≤ delta/长度 <269：用4位存13，扩展1字节存储（值-13）
// - delta/长度 ≥269：用4位存14，扩展2字节存储（值-269）
// - delta/长度=15：保留值，不支持
// 参数：delta - 选项号差值，length - 选项值长度
// 返回：头部字节、扩展delta（可选）、扩展长度（可选）
func (e *Encoder) encodeOptionHeader(delta, length uint16) (byte, []byte, []byte) {
	var header byte
	var extDelta, extLength []byte

	// 编码delta（选项号差值）
	if delta < 13 {
		// 直接用头部高4位存储delta
		header |= byte(delta) << 4
	} else if delta < 269 {
		// 扩展1字节：头部高4位存13，扩展字节存（delta-13）
		header |= 13 << 4
		extDelta = []byte{byte(delta - 13)}
	} else {
		// 扩展2字节：头部高4位存14，扩展字节存（delta-269）（大端序）
		header |= 14 << 4
		extDelta = make([]byte, 2)
		binary.BigEndian.PutUint16(extDelta, delta-269)
	}

	// 编码长度（选项值长度），逻辑与delta一致
	if length < 13 {
		// 直接用头部低4位存储长度
		header |= byte(length)
	} else if length < 269 {
		// 扩展1字节：头部低4位存13，扩展字节存（length-13）
		header |= 13
		extLength = []byte{byte(length - 13)}
	} else {
		// 扩展2字节：头部低4位存14，扩展字节存（length-269）（大端序）
		header |= 14
		extLength = make([]byte, 2)
		binary.BigEndian.PutUint16(extLength, length-269)
	}

	return header, extDelta, extLength
}

// readExtendedValue 读取扩展的delta或长度值（处理delta/长度为13/14的情况）
// 参数：reader - 字节流读取器，base - 基础值（13/14，来自选项头部的4位）
// 返回：实际的delta或长度值，若出错则返回错误信息
func (e *Encoder) readExtendedValue(reader *bytes.Reader, base uint16) (uint16, error) {
	switch base {
	case 13:
		// 1字节扩展：实际值 = 扩展字节值 + 13
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		return uint16(b) + 13, nil
	case 14:
		// 2字节扩展：实际值 = 扩展字节值（大端序） + 269
		var ext uint16
		if err := binary.Read(reader, binary.BigEndian, &ext); err != nil {
			return 0, err
		}
		return ext + 269, nil
	case 15:
		// 15为保留值，CoAP协议不支持
		return 0, fmt.Errorf("不支持的保留选项值15")
	default:
		// base <13：无需扩展，直接返回base
		return base, nil
	}
}

// validateMessage 验证CoAP消息结构体的合法性（避免无效格式）
// 检查项：协议版本、消息类型、令牌长度、令牌长度与实际令牌的匹配性
// 参数：msg - 待验证的CoAP消息结构体
// 返回：若合法则返回nil，否则返回错误信息
func (e *Encoder) validateMessage(msg *RawMessage) error {
	// 验证协议版本（仅支持v1）
	if msg.Version != Version1 {
		return fmt.Errorf("无效的协议版本: %d（仅支持CoAP v1）", msg.Version)
	}

	// 验证消息类型（仅支持0-3）
	if msg.Type > TypeReset {
		return fmt.Errorf("无效的消息类型: %d（仅支持0-3）", msg.Type)
	}

	// 验证令牌长度（CoAP协议限制最大8字节）
	if msg.TokenLength > 8 {
		return fmt.Errorf("令牌长度过长: %d（最大支持8字节）", msg.TokenLength)
	}

	// 验证令牌长度与实际令牌的匹配性（若声明长度>0，实际令牌需至少有对应长度）
	if msg.TokenLength > 0 && len(msg.Token) < int(msg.TokenLength) {
		return fmt.Errorf("令牌长度不匹配: 期望%d字节，实际%d字节", msg.TokenLength, len(msg.Token))
	}

	return nil
}

// Helper functions for common operations 通用操作辅助函数

// CreateMessage 创建一个默认配置的CoAP消息结构体
// 参数：messageType - 消息类型（TypeXXX常量），code - 消息码（方法码/状态码），messageID - 消息ID
// 返回：初始化后的RawMessage结构体（版本v1，无令牌，空选项列表）
func CreateMessage(messageType, code uint8, messageID uint16) *RawMessage {
	return &RawMessage{
		Version:     Version1,          // 固定为CoAP v1
		Type:        messageType,       // 传入的消息类型
		Code:        code,              // 传入的消息码
		MessageID:   messageID,         // 传入的消息ID
		TokenLength: 0,                 // 默认无令牌
		Options:     make([]Option, 0), // 默认空选项列表
	}
}

// AddOption 给CoAP消息添加一个选项（封装选项号与值的添加逻辑）
// 参数：number - 选项号（OptionXXX常量），value - 选项值字节流
func (msg *RawMessage) AddOption(number uint16, value []byte) {
	msg.Options = append(msg.Options, Option{
		Number: number,
		Value:  value,
	})
}

// SetPayload 给CoAP消息设置负载数据（直接覆盖原有负载）
// 参数：payload - 负载字节流（如JSON序列化后的业务数据）
func (msg *RawMessage) SetPayload(payload []byte) {
	msg.Payload = payload
}

// SetToken 给CoAP消息设置令牌（自动处理长度限制，超过8字节则截断）
// 参数：token - 令牌原始字节流
func (msg *RawMessage) SetToken(token []byte) {
	// 令牌最大8字节，超过则截断
	if len(token) > 8 {
		token = token[:8]
	}
	msg.Token = token
	// 同步更新令牌长度（与实际令牌长度一致）
	msg.TokenLength = uint8(len(token))
}

// GetOption 获取消息中第一个指定选项号的选项值（适用于单值选项）
// 参数：number - 目标选项号（OptionXXX常量）
// 返回：选项值字节流，bool值表示是否找到该选项
func (msg *RawMessage) GetOption(number uint16) ([]byte, bool) {
	for _, opt := range msg.Options {
		if opt.Number == number {
			return opt.Value, true
		}
	}
	// 未找到指定选项
	return nil, false
}

// GetOptions 获取消息中所有指定选项号的选项值（适用于多值选项，如Uri-Query）
// 参数：number - 目标选项号（OptionXXX常量）
// 返回：所有匹配选项的value列表（若未找到则返回空列表）
func (msg *RawMessage) GetOptions(number uint16) [][]byte {
	values := make([][]byte, 0)
	for _, opt := range msg.Options {
		if opt.Number == number {
			values = append(values, opt.Value)
		}
	}
	return values
}
