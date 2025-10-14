package logger

import "testing"

func Test_LOG(t *testing.T) {
	defer Sync()
	Info("Info msg")
	Warn("Warn msg")
	Error("Error msg")
	Debug("Debug msg", Int("age", 3))
}

