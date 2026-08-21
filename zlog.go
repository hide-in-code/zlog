package zlog

import (
	"os"
	"path"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	_loggers = make(map[string]*Logger)
	_mu      sync.RWMutex
)

// Logger is a zap-backed logger whose file output is page-cache aware:
// buffered writes are flushed to disk periodically, then fsync'd and
// evicted from the kernel page cache via posix_fadvise(POSIX_FADV_DONTNEED),
// so heavy log volume does not accumulate as cache memory in the cgroup.
type Logger struct {
	raw  *zap.Logger
	name string
}

// NewWithConf creates (or returns the cached instance of) the named logger.
// Only the existing logx.LogConf fields are used, no new config is required.
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
		_ = os.MkdirAll(logConf.Path, 0o755)
		w, err := newFileWriter(writerConfig{
			dir:        logConf.Path,
			name:       name,
			keepDays:   logConf.KeepDays,
			maxBackups: logConf.MaxBackups,
			maxSize:    int64(logConf.MaxSize) << 20, // unit is MB
			rotation:   logConf.Rotation,
		})
		if err != nil {
			writeSyncer = zapcore.AddSync(os.Stdout)
		} else {
			writeSyncer = w
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
		encCfg.EncodeTime = timeEncoder(logConf.TimeFormat)
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder = zapcore.NewConsoleEncoder(encCfg)
	default:
		encCfg := zap.NewProductionEncoderConfig()
		encCfg.EncodeTime = timeEncoder(logConf.TimeFormat)
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

func timeEncoder(timeFormat string) zapcore.TimeEncoder {
	if timeFormat != "" {
		return zapcore.TimeEncoderOfLayout(timeFormat)
	}
	return zapcore.ISO8601TimeEncoder
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

// Close flushes buffered entries to disk and drops the file page cache.
func (l *Logger) Close() error {
	return l.raw.Sync()
}

// CloseAll flushes and closes every cached logger.
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
	case "error", "fatal", "severe":
		// severe/fatal map to error instead of zap's FatalLevel so the
		// process is never os.Exit'ed with unflushed buffered logs.
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
