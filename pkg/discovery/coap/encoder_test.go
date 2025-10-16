package coap

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEncoder_EncodeDecodeHeader 测试CoAP消息头部的编码与解码功能
// 验证不同类型消息（确认型、非确认型、确认响应）的头部字段（版本、类型、令牌长度、消息码、消息ID、令牌）编解码后是否一致
func TestEncoder_EncodeDecodeHeader(t *testing.T) {
	// 测试用例集合：覆盖不同消息类型和令牌配置
	tests := []struct {
		name string      // 测试用例名称，描述场景
		msg  *RawMessage // 待测试的CoAP消息结构体
	}{
		{
			name: "Confirmable GET（确认型GET消息）",
			msg: &RawMessage{
				Version:     Version1,                       // CoAP v1版本
				Type:        TypeConfirmable,                // 确认型消息（需ACK响应）
				TokenLength: 4,                              // 令牌长度4字节
				Code:        0x01,                           // 消息码：GET请求（0.01）
				MessageID:   0x1234,                         // 消息ID：0x1234
				Token:       []byte{0x01, 0x02, 0x03, 0x04}, // 4字节令牌
			},
		},
		{
			name: "Non-Confirmable POST（非确认型POST消息）",
			msg: &RawMessage{
				Version:     Version1,           // CoAP v1版本
				Type:        TypeNonConfirmable, // 非确认型消息（无需响应）
				TokenLength: 0,                  // 无令牌
				Code:        0x02,               // 消息码：POST请求（0.02）
				MessageID:   0x5678,             // 消息ID：0x5678
			},
		},
		{
			name: "Acknowledgment（确认响应消息）",
			msg: &RawMessage{
				Version:     Version1,                                               // CoAP v1版本
				Type:        TypeAcknowledgment,                                     // 确认响应消息（ACK）
				TokenLength: 8,                                                      // 令牌长度8字节（最大支持长度）
				Code:        0x45,                                                   // 消息码：2.05 Content（成功响应）
				MessageID:   0x9ABC,                                                 // 消息ID：0x9ABC
				Token:       []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}, // 8字节令牌
			},
		},
	}

	// 创建编码器实例（所有测试用例共用）
	encoder := NewEncoder()

	// 遍历每个测试用例执行测试
	for _, tt := range tests {
		// t.Run：子测试，每个用例独立执行，便于定位失败场景
		t.Run(tt.name, func(t *testing.T) {
			// 1. 编码：将RawMessage转为字节流
			data, err := encoder.Encode(tt.msg)
			require.NoError(t, err)         // 断言编码无错误（若失败则终止用例）
			require.NotNil(t, data)         // 断言编码结果非空
			require.True(t, len(data) >= 4) // 断言字节流长度至少4字节（头部长度）

			// 2. 解码：将字节流转回RawMessage
			decoded, err := encoder.Decode(data)
			require.NoError(t, err)    // 断言解码无错误
			require.NotNil(t, decoded) // 断言解码结果非空

			// 3. 验证：解码后的字段与原始字段完全一致
			assert.Equal(t, tt.msg.Version, decoded.Version)         // 验证版本
			assert.Equal(t, tt.msg.Type, decoded.Type)               // 验证消息类型
			assert.Equal(t, tt.msg.TokenLength, decoded.TokenLength) // 验证令牌长度
			assert.Equal(t, tt.msg.Code, decoded.Code)               // 验证消息码
			assert.Equal(t, tt.msg.MessageID, decoded.MessageID)     // 验证消息ID

			// 若原始消息有令牌（长度>0），额外验证令牌内容
			if tt.msg.TokenLength > 0 {
				assert.Equal(t, tt.msg.Token, decoded.Token)
			}
		})
	}
}

// TestEncoder_Options 测试CoAP消息选项的编码与解码功能
// 验证多类型选项（UriHost、UriPort、UriPath等）添加后，解码能否正确还原选项数量和内容
func TestEncoder_Options(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 初始化基础消息：非确认型GET消息，消息ID=0x1234
	msg := CreateMessage(TypeNonConfirmable, 0x01, 0x1234)

	// 给消息添加多种常见选项（覆盖不同类型的选项）
	msg.AddOption(OptionUriHost, []byte("example.com"))           // Uri-Host：目标主机域名
	msg.AddOption(OptionUriPort, []byte{0x16, 0x33})              // Uri-Port：端口5683（0x1633=5683）
	msg.AddOption(OptionUriPath, []byte("test"))                  // Uri-Path：资源路径段1（"test"）
	msg.AddOption(OptionUriPath, []byte("path"))                  // Uri-Path：资源路径段2（"path"，多值选项）
	msg.AddOption(OptionContentFormat, []byte{ContentFormatJSON}) // Content-Format：负载格式为JSON（50）
	msg.AddOption(OptionMaxAge, []byte{0x00, 0x3C})               // Max-Age：资源缓存时间60秒（0x003C=60）

	// 1. 编码消息
	data, err := encoder.Encode(msg)
	require.NoError(t, err) // 断言编码无错误

	// 2. 解码消息
	decoded, err := encoder.Decode(data)
	require.NoError(t, err) // 断言解码无错误

	// 3. 验证选项：解码后的选项与原始配置一致
	assert.Len(t, decoded.Options, 6) // 验证选项总数为6个（与添加数量一致）

	// 验证Uri-Host选项（单值选项）
	host, found := decoded.GetOption(OptionUriHost)
	assert.True(t, found)                        // 断言选项存在
	assert.Equal(t, "example.com", string(host)) // 验证选项值

	// 验证Uri-Path选项（多值选项）
	paths := decoded.GetOptions(OptionUriPath)
	assert.Len(t, paths, 2)                   // 断言多值选项有2个值
	assert.Equal(t, "test", string(paths[0])) // 验证第一个路径段
	assert.Equal(t, "path", string(paths[1])) // 验证第二个路径段
}

// TestEncoder_Payload 测试CoAP消息负载的编码与解码功能
// 覆盖多种负载场景（空负载、短字符串、JSON、二进制、大负载），验证解码后负载完整性
func TestEncoder_Payload(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 测试用例集合：覆盖不同类型的负载
	tests := []struct {
		name    string // 测试用例名称
		payload []byte // 待测试的负载数据
	}{
		{
			name:    "Empty payload（空负载）",
			payload: nil, // 无负载
		},
		{
			name:    "Small payload（短字符串负载）",
			payload: []byte("Hello, World!"), // 简单字符串
		},
		{
			name:    "JSON payload（JSON格式负载）",
			payload: []byte(`{"device":"test","version":"1.0"}`), // 设备信息JSON
		},
		{
			name:    "Binary payload（二进制负载）",
			payload: []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}, // 8字节二进制数据
		},
		{
			name:    "Large payload（大负载）",
			payload: bytes.Repeat([]byte("A"), 1024), // 1024字节重复"A"（模拟大负载）
		},
	}

	// 遍历每个测试用例执行测试
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 初始化消息：非确认型POST消息，消息ID=0x5678
			msg := CreateMessage(TypeNonConfirmable, 0x02, 0x5678)
			msg.SetPayload(tt.payload) // 设置负载

			// 1. 编码消息
			data, err := encoder.Encode(msg)
			require.NoError(t, err) // 断言编码无错误

			// 2. 解码消息
			decoded, err := encoder.Decode(data)
			require.NoError(t, err) // 断言解码无错误

			// 3. 验证负载：解码后的负载与原始负载一致
			if tt.payload == nil {
				assert.Nil(t, decoded.Payload) // 空负载验证
			} else {
				assert.Equal(t, tt.payload, decoded.Payload) // 非空负载验证（字节完全匹配）
			}
		})
	}
}

// TestEncoder_DeltaEncoding 测试CoAP选项的delta编码功能
// CoAP选项需按"选项号增量"编码，此用例验证常规delta、需扩展delta的场景能否正确编解码
func TestEncoder_DeltaEncoding(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 初始化基础消息：非确认型GET消息，消息ID=0x1234
	msg := CreateMessage(TypeNonConfirmable, 0x01, 0x1234)

	// 添加选项：选项号递增，覆盖不同delta场景
	msg.AddOption(1, []byte{0x01})         // 选项1（If-Match）：初始delta=1（前一选项号0）
	msg.AddOption(3, []byte{0x03})         // 选项3（Uri-Host）：delta=2（3-1=2）
	msg.AddOption(7, []byte{0x07})         // 选项7（Uri-Port）：delta=4（7-3=4）
	msg.AddOption(11, []byte{0x0B})        // 选项11（Uri-Path）：delta=4（11-7=4）
	msg.AddOption(15, []byte{0x0F})        // 选项15（Uri-Query）：delta=4（15-11=4）
	msg.AddOption(20, []byte{0x14})        // 选项20（Location-Query）：delta=5（20-15=5）
	msg.AddOption(35, []byte{0x23})        // 选项35（Proxy-Uri）：delta=15（35-20=15，需1字节扩展）
	msg.AddOption(300, []byte{0x01, 0x2C}) // 选项300：delta=265（300-35=265，需2字节扩展）

	// 1. 编码消息
	data, err := encoder.Encode(msg)
	require.NoError(t, err) // 断言编码无错误

	// 2. 解码消息
	decoded, err := encoder.Decode(data)
	require.NoError(t, err) // 断言解码无错误

	// 3. 验证选项：delta编码后的选项能正确还原
	assert.Len(t, decoded.Options, 8) // 验证选项总数为8个

	// 验证选项1（If-Match）
	val, found := decoded.GetOption(1)
	assert.True(t, found)
	assert.Equal(t, []byte{0x01}, val)

	// 验证选项300（大delta场景）
	val, found = decoded.GetOption(300)
	assert.True(t, found)
	assert.Equal(t, []byte{0x01, 0x2C}, val)
}

// TestEncoder_InvalidMessages 测试无效CoAP消息的编码行为
// 验证不符合协议规范的消息（如nil消息、版本错误、令牌过长）编码时能否正确返回错误
func TestEncoder_InvalidMessages(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 测试用例集合：覆盖多种无效消息场景
	tests := []struct {
		name    string      // 测试用例名称
		msg     *RawMessage // 无效消息结构体
		wantErr bool        // 预期是否返回错误（true=应报错，false=应成功）
	}{
		{
			name:    "Nil message（nil消息）",
			msg:     nil,  // 消息为nil
			wantErr: true, // 预期报错
		},
		{
			name: "Invalid version（无效版本）",
			msg: &RawMessage{
				Version:   2,                  // 版本=2（仅支持v1）
				Type:      TypeNonConfirmable, // 非确认型消息
				Code:      0x01,               // GET请求
				MessageID: 0x1234,             // 消息ID
			},
			wantErr: true, // 预期报错
		},
		{
			name: "Invalid type（无效消息类型）",
			msg: &RawMessage{
				Version:   Version1, // 正确版本
				Type:      5,        // 类型=5（仅支持0-3）
				Code:      0x01,     // GET请求
				MessageID: 0x1234,   // 消息ID
			},
			wantErr: true, // 预期报错
		},
		{
			name: "Token too long（令牌过长）",
			msg: &RawMessage{
				Version:     Version1,                      // 正确版本
				Type:        TypeNonConfirmable,            // 非确认型消息
				Code:        0x01,                          // GET请求
				MessageID:   0x1234,                        // 消息ID
				TokenLength: 9,                             // 令牌长度=9（最大支持8）
				Token:       bytes.Repeat([]byte{0x01}, 9), // 9字节令牌
			},
			wantErr: true, // 预期报错
		},
		{
			name: "Token length mismatch（令牌长度不匹配）",
			msg: &RawMessage{
				Version:     Version1,           // 正确版本
				Type:        TypeNonConfirmable, // 非确认型消息
				Code:        0x01,               // GET请求
				MessageID:   0x1234,             // 消息ID
				TokenLength: 4,                  // 声明令牌长度=4
				Token:       []byte{0x01, 0x02}, // 实际令牌仅2字节
			},
			wantErr: true, // 预期报错
		},
	}

	// 遍历每个测试用例执行测试
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 执行编码操作
			_, err := encoder.Encode(tt.msg)
			// 验证错误结果是否符合预期
			if tt.wantErr {
				assert.Error(t, err) // 预期报错，断言有错误
			} else {
				assert.NoError(t, err) // 预期成功，断言无错误
			}
		})
	}
}

// TestEncoder_DecodeInvalidData 测试无效字节流的解码行为
// 验证不符合CoAP格式的字节流（如空数据、过短数据、版本错误）解码时能否正确返回错误
func TestEncoder_DecodeInvalidData(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 测试用例集合：覆盖多种无效字节流场景
	tests := []struct {
		name    string // 测试用例名称
		data    []byte // 无效字节流
		wantErr bool   // 预期是否返回错误
	}{
		{
			name:    "Empty data（空数据）",
			data:    []byte{}, // 无字节数据
			wantErr: true,     // 预期报错（头部至少4字节）
		},
		{
			name:    "Too short（数据过短）",
			data:    []byte{0x01, 0x02}, // 仅2字节（不足头部4字节）
			wantErr: true,               // 预期报错
		},
		{
			name:    "Invalid version（版本错误数据）",
			data:    []byte{0x80, 0x01, 0x00, 0x00}, // 版本=2（0x80的高2位为10=2）
			wantErr: true,                           // 预期报错（仅支持v1）
		},
		{
			name:    "Valid minimal message（合法最小消息）",
			data:    []byte{0x40, 0x01, 0x00, 0x00}, // 版本=1、类型=NON、无令牌、GET、消息ID=0
			wantErr: false,                          // 预期成功（合法消息）
		},
	}

	// 遍历每个测试用例执行测试
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 执行解码操作
			_, err := encoder.Decode(tt.data)
			// 验证错误结果是否符合预期
			if tt.wantErr {
				assert.Error(t, err) // 预期报错，断言有错误
			} else {
				assert.NoError(t, err) // 预期成功，断言无错误
			}
		})
	}
}

// TestEncoder_RoundTrip 测试CoAP消息的"编码-解码-再编码"往返一致性
// 验证消息经过多次编解码后，数据是否完全一致（确保编解码逻辑无副作用）
func TestEncoder_RoundTrip(t *testing.T) {
	// 创建编码器实例
	encoder := NewEncoder()

	// 1. 构建一个复杂的CoAP消息（包含令牌、多选项、JSON负载）
	msg := CreateMessage(TypeConfirmable, 0x02, 0xABCD) // 确认型POST消息，消息ID=0xABCD
	msg.SetToken([]byte{0xDE, 0xAD, 0xBE, 0xEF})        // 4字节令牌（DE AD BE EF）
	// 添加多个选项
	msg.AddOption(OptionUriHost, []byte("coap.example.com"))      // Uri-Host：coap.example.com
	msg.AddOption(OptionUriPort, []byte{0x16, 0x33})              // Uri-Port：5683
	msg.AddOption(OptionUriPath, []byte("api"))                   // Uri-Path：api
	msg.AddOption(OptionUriPath, []byte("v1"))                    // Uri-Path：v1
	msg.AddOption(OptionUriPath, []byte("devices"))               // Uri-Path：devices
	msg.AddOption(OptionUriQuery, []byte("filter=active"))        // Uri-Query：filter=active
	msg.AddOption(OptionContentFormat, []byte{ContentFormatJSON}) // Content-Format：JSON
	msg.AddOption(OptionAccept, []byte{ContentFormatJSON})        // Accept：期望响应为JSON
	msg.SetPayload([]byte(`{"action":"discover","timeout":30}`))  // 负载：设备发现JSON

	// 2. 第一次往返：编码→解码
	data1, err := encoder.Encode(msg)
	require.NoError(t, err) // 断言第一次编码无错误

	decoded1, err := encoder.Decode(data1)
	require.NoError(t, err) // 断言第一次解码无错误

	// 3. 第二次往返：解码结果再编码→再解码
	data2, err := encoder.Encode(decoded1)
	require.NoError(t, err) // 断言第二次编码无错误

	decoded2, err := encoder.Decode(data2)
	require.NoError(t, err) // 断言第二次解码无错误

	// 4. 验证一致性
	assert.Equal(t, data1, data2) // 两次编码的字节流完全一致

	// 两次解码的消息字段完全一致
	assert.Equal(t, decoded1.Version, decoded2.Version)
	assert.Equal(t, decoded1.Type, decoded2.Type)
	assert.Equal(t, decoded1.Code, decoded2.Code)
	assert.Equal(t, decoded1.MessageID, decoded2.MessageID)
	assert.Equal(t, decoded1.Token, decoded2.Token)
	assert.Equal(t, decoded1.Payload, decoded2.Payload)
	assert.Equal(t, len(decoded1.Options), len(decoded2.Options)) // 选项数量一致
}

// BenchmarkEncoder_Encode 基准测试：CoAP消息编码性能
// 统计每秒能编码多少次消息（评估编码逻辑的性能）
func BenchmarkEncoder_Encode(b *testing.B) {
	// 创建编码器实例
	encoder := NewEncoder()
	// 构建一个基础消息（含选项和负载）
	msg := CreateMessage(TypeNonConfirmable, 0x01, 0x1234) // 非确认型GET消息
	msg.AddOption(OptionUriPath, []byte("test"))           // 添加Uri-Path选项
	msg.SetPayload([]byte("Hello, World!"))                // 设置短字符串负载

	b.ResetTimer() // 重置计时器（排除初始化时间干扰）
	// 循环b.N次（b.N由测试框架根据性能自动调整）
	for i := 0; i < b.N; i++ {
		_, _ = encoder.Encode(msg) // 执行编码（忽略错误，仅测性能）
	}
}

// BenchmarkEncoder_Decode 基准测试：CoAP消息解码性能
// 统计每秒能解码多少次消息（评估解码逻辑的性能）
func BenchmarkEncoder_Decode(b *testing.B) {
	// 创建编码器实例
	encoder := NewEncoder()
	// 先构建并编码一个基础消息（用于后续解码测试）
	msg := CreateMessage(TypeNonConfirmable, 0x01, 0x1234)
	msg.AddOption(OptionUriPath, []byte("test"))
	msg.SetPayload([]byte("Hello, World!"))
	data, _ := encoder.Encode(msg) // 预编码得到字节流

	b.ResetTimer() // 重置计时器（排除初始化时间干扰）
	// 循环b.N次，重复解码同一字节流
	for i := 0; i < b.N; i++ {
		_, _ = encoder.Decode(data) // 执行解码（忽略错误，仅测性能）
	}
}
