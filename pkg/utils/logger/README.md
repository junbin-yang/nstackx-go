# logger

基于 [zap](https://github.com/uber-go/zap) 的二次封装，提升 zap 使用体验。

## 使用示例

### 开箱即用

```go
package main

import (
	"os"
	"time"

	log "github.com/junbin-yang/nstackx-go/pkg/utils/logger"
)

func main() {
	defer log.Sync()
	log.Info("Info msg")
	log.Warn("Warn msg", log.Int("attempt", 3))
	log.Error("Error msg", log.Duration("backoff", time.Second))

	// 修改日志级别
	log.SetLevel(log.ErrorLevel)
	log.Info("Info msg")
	log.Warn("Warn msg")
	log.Error("Error msg")

	// 替换默认 Logger
	file, _ := os.OpenFile("custom.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	logger := log.New(file, log.InfoLevel)
	log.ReplaceDefault(logger)
	log.Info("Info msg in replace default logger after")
}
```

### 选项

支持 [zap 选项](https://pkg.go.dev/go.uber.org/zap#Option)

```go
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

    log "github.com/junbin-yang/nstackx-go/pkg/utils/logger"
)

func main() {
	file, _ := os.OpenFile("test.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	opts := []log.Option{
		// 附加日志调用信息
		log.WithCaller(true),
		log.AddCallerSkip(1),
		// Warn 级别日志 Hook
		log.Hooks(func(entry zapcore.Entry) error {
			if entry.Level == log.WarnLevel {
				fmt.Printf("Warn Hook: msg=%s\n", entry.Message)
			}
			return nil
		}),
		// Fatal 级别日志 Hook
		zap.WithFatalHook(Hook{}),
	}
	logger := log.New(io.MultiWriter(os.Stdout, file), log.InfoLevel, opts...)
	defer logger.Sync()

	logger.Info("Info msg", log.String("val", "string"))
	logger.Warn("Warn msg", log.Int("val", 7))
	logger.Fatal("Fatal msg", log.Time("val", time.Now()))
}

type Hook struct{}

func (h Hook) OnWrite(ce *zapcore.CheckedEntry, field []zapcore.Field) {
	fmt.Printf("Fatal Hook: msg=%s, field=%+v\n", ce.Message, field)
}
```

### 日志轮转

`Warn` 以下级别日志按大小轮转，`Warn` 及以上级别日志按时间轮转。

```go
package main

import (
    log "github.com/junbin-yang/nstackx-go/pkg/utils/logger"
)

func main() {
    out := log.NewProductionRotateByTime("rotate-by-time.log")
    logger := log.New(out, log.InfoLevel)
    log.ReplaceDefault(logger)
    defer log.Sync()

	log.Debug("Debug msg")
	log.Info("Info msg")
	log.Warn("Warn msg")
	log.Error("Error msg")
}
```

