package zlog

import (
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	_loggers = make(map[string]*Logger)
	_mu      sync.RWMutex
)

type Logger struct {
	raw  *zap.Logger
	name string
}

func NewWithConf(name string, conf logx.LogConf) *Logger {
	logConf := conf
	logConf.Path = path.Join(logConf.Path, name)
	return createLogger(name, logConf)
}

func createLogger(name string, logConf logx.LogConf) *Logger {
	_mu.RLock()
	if l, ok := _loggers[name]; ok {
		_mu.RUnlock()
		return l
	}
	_mu.RUnlock()

	_mu.Lock()
	defer _mu.Unlock()

	if l, ok := _loggers[name]; ok {
		return l
	}

	var writeSyncer zapcore.WriteSyncer

	if logConf.Mode == "file" || logConf.Mode == "volume" {
		_ = os.MkdirAll(logConf.Path, 0755)

		pattern := path.Join(logConf.Path, fmt.Sprintf("%s-%%Y-%%m-%%d.log", name))

		linkName := path.Join(logConf.Path, fmt.Sprintf("%s.log", name))

		rotator, err := rotatelogs.New(
			pattern,
			rotatelogs.WithLinkName(linkName),
			rotatelogs.WithMaxAge(time.Duration(logConf.KeepDays)*24*time.Hour),
			rotatelogs.WithRotationTime(24*time.Hour),
			rotatelogs.WithClock(rotatelogs.Local),
		)
		if err != nil {
			writeSyncer = zapcore.AddSync(os.Stdout)
		} else {
			writeSyncer = zapcore.AddSync(rotator)
		}
	} else {
		writeSyncer = zapcore.AddSync(os.Stdout)
	}

	level := parseLevel(logConf.Level)

	encoding := logConf.Encoding
	if encoding == "" {
		encoding = "json"
	}

	var encoder zapcore.Encoder
	switch encoding {
	case "plain":
		encCfg := zap.NewDevelopmentEncoderConfig()
		encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder = zapcore.NewConsoleEncoder(encCfg)
	default:
		encCfg := zap.NewProductionEncoderConfig()
		encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encCfg.EncodeDuration = zapcore.SecondsDurationEncoder
		encoder = zapcore.NewJSONEncoder(encCfg)
	}

	core := zapcore.NewCore(encoder, writeSyncer, level)

	opts := []zap.Option{
		zap.AddCaller(),
		zap.AddCallerSkip(1),
		zap.Fields(zap.String("module", name)),
	}

	l := &Logger{
		raw:  zap.New(core, opts...),
		name: name,
	}

	_loggers[name] = l
	return l
}

func (l *Logger) Info(msg string, fields ...zap.Field) {
	l.raw.Info(msg, fields...)
}

func (l *Logger) Warn(msg string, fields ...zap.Field) {
	l.raw.Warn(msg, fields...)
}

func (l *Logger) Error(msg string, fields ...zap.Field) {
	l.raw.Error(msg, fields...)
}

func (l *Logger) Debug(msg string, fields ...zap.Field) {
	l.raw.Debug(msg, fields...)
}

func (l *Logger) Close() error {
	return l.raw.Sync()
}

func CloseAll() {
	_mu.Lock()
	defer _mu.Unlock()
	for _, logger := range _loggers {
		_ = logger.Close()
	}
	_loggers = make(map[string]*Logger)
}

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
