package zlog

import "time"

// Options 控制 zlog 底层文件写入器的行为。
type Options struct {
	// MaxSizeMB 单个日志文件的最大体积（MB），超过后按大小轮转；0 表示不限制。
	MaxSizeMB int
	// RotationInterval 按时间轮转的间隔；0 表示不启用时间轮转。
	RotationInterval time.Duration
	// BufferSize 内存写缓冲大小（字节）；0 使用默认 256KB。
	BufferSize int
	// FlushInterval 缓冲刷新间隔；0 使用默认 1s。
	FlushInterval time.Duration
	// FadviseEnabled 是否启用 fadvise(FADV_DONTNEED) 释放 Page Cache。
	FadviseEnabled bool
	// FadviseInterval 两次 fadvise 之间的最小时间间隔；0 表示不按时间限制。
	FadviseInterval time.Duration
	// FadviseMinBytes 距上次 fadvise 累计写入超过该字节数才再次触发；0 表示不按字节限制。
	// 该值同时是内存硬顶的依据：累计未释放数据达到 2 倍该值时无视时间间隔立即释放，
	// 因此 Page Cache 占用上限约为 max(2*FadviseMinBytes, 写入速率*FadviseInterval)。
	FadviseMinBytes int64
	// SyncBeforeFadvise 在 fadvise 前先 fdatasync，确保脏页回写为干净页后才能被回收。
	SyncBeforeFadvise bool
	// MaxBackups 保留的最大历史文件数；0 表示不限制。
	MaxBackups int
}

// defaultOptions 返回生产环境推荐的默认配置：
// 缓冲 256KB/1s 批量落盘，按 1 小时或 100MB 轮转，
// 每 10s 或累计 4MB 执行一次 fdatasync + fadvise 释放 Page Cache。
func defaultOptions() Options {
	return Options{
		MaxSizeMB:         100,
		RotationInterval:  1 * time.Hour,
		BufferSize:        256 * 1024,
		FlushInterval:     time.Second,
		FadviseEnabled:    true,
		FadviseInterval:   10 * time.Second,
		FadviseMinBytes:   4 * 1024 * 1024,
		SyncBeforeFadvise: true,
		MaxBackups:        100,
	}
}

// Option 以函数方式覆盖默认 Options。
type Option func(*Options)

// WithFadviseEnabled 设置是否启用 fadvise 释放 Page Cache。
func WithFadviseEnabled(enabled bool) Option {
	return func(o *Options) { o.FadviseEnabled = enabled }
}

// WithFadviseInterval 设置两次 fadvise 之间的最小时间间隔。
func WithFadviseInterval(d time.Duration) Option {
	return func(o *Options) { o.FadviseInterval = d }
}

// WithFadviseMinBytes 设置触发 fadvise 的最小累计写入字节数。
func WithFadviseMinBytes(n int64) Option {
	return func(o *Options) { o.FadviseMinBytes = n }
}

// WithSyncBeforeFadvise 设置是否在 fadvise 前先 fdatasync。
func WithSyncBeforeFadvise(sync bool) Option {
	return func(o *Options) { o.SyncBeforeFadvise = sync }
}

// WithMaxSizeMB 设置按大小轮转的阈值（MB），0 表示不限制。
func WithMaxSizeMB(mb int) Option {
	return func(o *Options) { o.MaxSizeMB = mb }
}

// WithRotationInterval 设置按时间轮转的间隔，0 表示禁用。
func WithRotationInterval(d time.Duration) Option {
	return func(o *Options) { o.RotationInterval = d }
}

// WithMaxBackups 设置保留的最大历史文件数，0 表示不限制。
func WithMaxBackups(n int) Option {
	return func(o *Options) { o.MaxBackups = n }
}
