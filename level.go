package zlog

import "go.uber.org/zap/zapcore"

// parseLevel 将字符串日志级别转换为 zapcore.Level
func parseLevel(levelStr string) zapcore.Level {
	switch levelStr {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	case "fatal", "severe":
		return zapcore.FatalLevel
	default:
		return zapcore.InfoLevel
	}
}
