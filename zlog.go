package zlog

import (
	"os"
	"path"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	_loggers = make(map[string]*Logger)
	_mu      sync.RWMutex
)

// Logger 封装了 zap.Logger，提供带模块名的日志能力。
type Logger struct {
	raw      *zap.Logger
	name     string
	writer   *fadviseRotatingWriter       // 持有引用，用于 Close 时释放资源
	buffered *zapcore.BufferedWriteSyncer // 持有引用，Close 时停止后台刷新协程
}

// NewWithConf 根据模块名、go-zero 的 LogConf 与可选 Options 创建 Logger。
// Options 用于覆盖默认的缓冲、轮转与 fadvise 节流参数。
func NewWithConf(name string, conf logx.LogConf, opts ...Option) *Logger {
	logConf := conf
	logConf.Path = path.Join(logConf.Path, name)
	return createLogger(name, logConf, opts...)
}

func createLogger(name string, logConf logx.LogConf, opts ...Option) *Logger {
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
	var rotatingWriter *fadviseRotatingWriter
	var buffered *zapcore.BufferedWriteSyncer

	if logConf.Mode == "file" || logConf.Mode == "volume" {
		_ = os.MkdirAll(logConf.Path, 0755)

		o := defaultOptions()
		applyConfDefaults(&o, logConf)
		for _, fn := range opts {
			fn(&o)
		}

		var maxAge time.Duration
		if logConf.KeepDays > 0 {
			maxAge = time.Duration(logConf.KeepDays) * 24 * time.Hour
		}

		rotatingWriter = newFadviseRotatingWriter(logConf.Path, name, maxAge, o)
		buffered = &zapcore.BufferedWriteSyncer{
			WS:            rotatingWriter,
			Size:          o.BufferSize,
			FlushInterval: o.FlushInterval,
		}
		writeSyncer = buffered
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

	zapOpts := []zap.Option{
		zap.AddCaller(),
		zap.AddCallerSkip(1),
		zap.Fields(zap.String("module", name)),
	}

	l := &Logger{
		raw:      zap.New(core, zapOpts...),
		name:     name,
		writer:   rotatingWriter,
		buffered: buffered,
	}

	_loggers[name] = l
	return l
}

// applyConfDefaults 将 go-zero LogConf 中的轮转参数映射到 Options，
// 用户通过 Option 传入的值优先。
func applyConfDefaults(o *Options, conf logx.LogConf) {
	switch conf.Rotation {
	case "size":
		o.RotationInterval = 0
		if conf.MaxSize > 0 {
			o.MaxSizeMB = conf.MaxSize
		}
	case "daily":
		o.RotationInterval = 24 * time.Hour
		o.MaxSizeMB = 0
	}
	if conf.MaxBackups > 0 {
		o.MaxBackups = conf.MaxBackups
	}
}

// Info 记录 Info 级别日志
func (l *Logger) Info(msg string, fields ...zap.Field) {
	l.raw.Info(msg, fields...)
}

// Warn 记录 Warn 级别日志
func (l *Logger) Warn(msg string, fields ...zap.Field) {
	l.raw.Warn(msg, fields...)
}

// Error 记录 Error 级别日志
func (l *Logger) Error(msg string, fields ...zap.Field) {
	l.raw.Error(msg, fields...)
}

// Debug 记录 Debug 级别日志
func (l *Logger) Debug(msg string, fields ...zap.Field) {
	l.raw.Debug(msg, fields...)
}

// Stats 返回当前 Logger 底层写入器的运行统计；控制台模式下返回零值。
func (l *Logger) Stats() Stats {
	if l.writer == nil {
		return Stats{}
	}
	return l.writer.Stats()
}

// LastError 返回底层写入器最近一次写入/轮转错误，用于磁盘故障等场景的监控。
func (l *Logger) LastError() error {
	if l.writer == nil {
		return nil
	}
	return l.writer.LastError()
}

// Close 关闭 Logger，停止后台刷新协程并释放底层文件资源。
func (l *Logger) Close() error {
	// 先停止缓冲刷新协程并刷出残余数据，再关闭文件
	if l.buffered != nil {
		_ = l.buffered.Stop()
	}
	_ = l.raw.Sync()
	if l.writer != nil {
		return l.writer.Close()
	}
	return nil
}

// CloseAll 关闭所有已创建的 Logger
func CloseAll() {
	_mu.Lock()
	defer _mu.Unlock()
	for _, logger := range _loggers {
		_ = logger.Close()
	}
	_loggers = make(map[string]*Logger)
}
