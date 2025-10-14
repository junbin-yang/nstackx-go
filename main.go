package main

import (
	"time"
	log "github.com/junbin-yang/nstackx-go/pkg/utils/logger"
)

func main() {
	defer log.Sync()
	log.Info("failed to fetch URL", log.String("url", "https://jianghushinian.cn/"))
	log.Warn("Warn msg", log.Int("attempt", 3))
	log.Error("Error msg", log.Duration("backoff", time.Second))

	log.SetLevel(log.ErrorLevel)
	log.Info("Info msg")
	log.Warn("Warn msg")
	log.Error("Error msg")
}
