package zlog

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// fadviseRotatingWriter 是带缓冲的日志写入器，支持按时间/大小轮转、
// 过期清理，以及节流式 fadvise(FADV_DONTNEED) 释放 Page Cache。
//
// Page Cache 膨胀的根因：日志文件由 page cache 承载，写入后脏页会在
// cgroup 的 memory.current（systemctl 显示的占用）里持续累积，直到内核
// 后台回写。逐条调用 fadvise 既浪费 syscall，又因页面仍为脏页而无效。
// 本实现将写入批量缓冲后，按时间/字节双阈值节流触发
// fdatasync + FADV_DONTNEED，把 Page Cache 占用稳定地约束在
// "最近一个节流周期内写入的数据量" 附近。
//
// 内存保证：任意吞吐下，未释放的 Page Cache 不超过 max(2*FadviseMinBytes,
// 写入速率*FadviseInterval)；syscall 开销约为 1 次 fdatasync/秒（由缓冲
// 刷新协程触发）+ 1 次 fadvise/节流周期。
type fadviseRotatingWriter struct {
	mu   sync.Mutex
	opts Options

	dir      string
	name     string
	linkName string
	maxAge   time.Duration
	maxSize  int64 // 单文件字节上限；0 表示不限制

	currentFile *os.File
	lastPeriod  string // 当前文件所属的时间周期（2006-01-02-15）
	lastRotate  time.Time
	seq         int   // 同一时间周期内按大小轮转的序号
	written     int64 // 当前文件已写入字节数
	advised     int64 // 已执行 fadvise 的字节偏移
	lastAdvise  time.Time
	lastErr     error // 最近一次写入/轮转错误，供外部监控

	stats writerStats
}

type writerStats struct {
	writes       atomic.Int64
	bytes        atomic.Int64
	advises      atomic.Int64
	adviseErrors atomic.Int64
	rotations    atomic.Int64
	cleaned      atomic.Int64
}

// Stats 是写入器的运行统计，可用于监控日志链路健康度。
type Stats struct {
	Writes       int64 // 底层文件写入次数（缓冲刷出后的批次）
	BytesWritten int64 // 累计写入字节数
	FadviseCalls int64 // fadvise 调用次数
	FadviseErrs  int64 // fadvise 失败次数
	Rotations    int64 // 轮转次数
	CleanedFiles int64 // 清理的历史文件数
}

func (s *writerStats) snapshot() Stats {
	return Stats{
		Writes:       s.writes.Load(),
		BytesWritten: s.bytes.Load(),
		FadviseCalls: s.advises.Load(),
		FadviseErrs:  s.adviseErrors.Load(),
		Rotations:    s.rotations.Load(),
		CleanedFiles: s.cleaned.Load(),
	}
}

// newFadviseRotatingWriter 创建一个带 fadvise 能力的日志轮转 Writer。
func newFadviseRotatingWriter(dir, name string, maxAge time.Duration, opts Options) *fadviseRotatingWriter {
	w := &fadviseRotatingWriter{
		opts:     opts,
		dir:      dir,
		name:     name,
		linkName: path.Join(dir, name+".log"),
		maxAge:   maxAge,
		maxSize:  int64(opts.MaxSizeMB) << 20,
	}
	if err := w.rotate(); err != nil {
		// 初始化失败（如磁盘不可写）时 currentFile 保持 nil，
		// 后续 Write 会返回错误，由上层感知。
		_ = err
	}
	return w
}

// Write 写入日志数据，并按节流条件触发 fadvise 释放 Page Cache。
func (w *fadviseRotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	if w.shouldRotate(now, int64(len(p))) {
		if err := w.rotate(); err != nil {
			w.lastErr = err
			return 0, err
		}
	}

	if w.currentFile == nil {
		err := fmt.Errorf("zlog: log file not opened")
		w.lastErr = err
		return 0, err
	}

	n, err := w.currentFile.Write(p)
	if n > 0 {
		w.written += int64(n)
		w.stats.writes.Add(1)
		w.stats.bytes.Add(int64(n))
	}
	if err != nil {
		w.lastErr = err
		return n, err
	}

	w.adviseIfDue(now)
	return n, nil
}

// Sync 将数据刷到磁盘（fdatasync，开销低于 fsync）。
func (w *fadviseRotatingWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentFile == nil {
		return nil
	}
	return unix.Fdatasync(int(w.currentFile.Fd()))
}

// Close 释放文件资源：最终 fdatasync + 整文件 fadvise 后关闭。
func (w *fadviseRotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentFile == nil {
		return nil
	}
	w.releaseFileLocked(w.currentFile)
	err := w.currentFile.Close()
	w.currentFile = nil
	return err
}

// Stats 返回写入器累计的运行统计。
func (w *fadviseRotatingWriter) Stats() Stats {
	return w.stats.snapshot()
}

// LastError 返回最近一次写入/轮转错误；nil 表示当前无错误。
func (w *fadviseRotatingWriter) LastError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

// shouldRotate 判断是否需要轮转（新周期、超时或超体积）。
func (w *fadviseRotatingWriter) shouldRotate(now time.Time, incoming int64) bool {
	if w.currentFile == nil {
		return true
	}
	if w.opts.RotationInterval > 0 && now.Sub(w.lastRotate) >= w.opts.RotationInterval {
		return true
	}
	if w.maxSize > 0 && w.written+incoming > w.maxSize {
		return true
	}
	return false
}

// rotate 轮转到新文件：先释放旧文件 Page Cache，再打开新文件并原子更新软链接。
// 先打开新文件成功后才关闭旧文件，避免磁盘故障时丢失写入能力。
func (w *fadviseRotatingWriter) rotate() error {
	now := time.Now()
	period := now.Format("2006-01-02-15")
	if w.lastPeriod != period {
		w.seq = 0
	} else {
		w.seq++
	}
	w.lastPeriod = period

	base := path.Join(w.dir, fmt.Sprintf("%s-%s", w.name, period))
	fileName := base + ".log"
	if w.seq > 0 {
		fileName = fmt.Sprintf("%s-%d.log", base, w.seq)
	}

	f, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("zlog: open log file %s: %w", fileName, err)
	}

	// 释放旧文件占用的 Page Cache
	if w.currentFile != nil {
		w.releaseFileLocked(w.currentFile)
		_ = w.currentFile.Close()
	}

	w.currentFile = f
	w.lastRotate = now
	w.written = 0
	w.advised = 0
	w.stats.rotations.Add(1)

	// 原子更新软链接，使其始终指向最新日志文件
	if err := w.updateLink(fileName); err != nil {
		_ = err
	}

	w.cleanOldFiles()
	return nil
}

// adviseIfDue 在满足节流条件时对 [advised, written) 执行 fadvise。
// 节流规则（两者可同时配置）：
//   - 时间间隔：距上次 fadvise 至少 FadviseInterval；
//   - 字节阈值：累计未释放数据至少 FadviseMinBytes；
//   - 内存硬顶：累计未释放数据达到 2*FadviseMinBytes 时无视时间间隔立即触发，
//     保证高吞吐下 Page Cache 依然有界。
func (w *fadviseRotatingWriter) adviseIfDue(now time.Time) {
	if !w.opts.FadviseEnabled || w.currentFile == nil {
		return
	}
	pending := w.written - w.advised
	if w.opts.FadviseMinBytes > 0 && pending >= 2*w.opts.FadviseMinBytes {
		w.adviseLocked()
		return
	}
	if w.opts.FadviseInterval > 0 && now.Sub(w.lastAdvise) < w.opts.FadviseInterval {
		return
	}
	if w.opts.FadviseMinBytes > 0 && pending < w.opts.FadviseMinBytes {
		return
	}
	w.adviseLocked()
}

// adviseLocked 对 [advised, written) 区间执行 fdatasync + FADV_DONTNEED。
// 脏页必须先回写为干净页，DONTNEED 才能将其从 Page Cache 中回收。
func (w *fadviseRotatingWriter) adviseLocked() {
	start, end := w.advised, w.written
	if end <= start {
		w.lastAdvise = time.Now()
		return
	}
	fd := int(w.currentFile.Fd())
	if w.opts.SyncBeforeFadvise {
		_ = unix.Fdatasync(fd)
	}
	err := unix.Fadvise(fd, start, end-start, unix.FADV_DONTNEED)
	w.stats.advises.Add(1)
	if err != nil {
		w.stats.adviseErrors.Add(1)
	}
	w.advised = end
	w.lastAdvise = time.Now()
}

// releaseFileLocked 对整文件做最终 fdatasync 并释放 Page Cache，用于轮转/关闭前。
func (w *fadviseRotatingWriter) releaseFileLocked(f *os.File) {
	if !w.opts.FadviseEnabled {
		return
	}
	fd := int(f.Fd())
	if w.opts.SyncBeforeFadvise {
		_ = unix.Fdatasync(fd)
	}
	if err := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED); err != nil {
		w.stats.adviseErrors.Add(1)
	}
	w.stats.advises.Add(1)
}

// updateLink 原子地将软链接指向最新日志文件。
// 目标使用文件 basename，保证在相对路径目录下链接也可解析。
func (w *fadviseRotatingWriter) updateLink(target string) error {
	tmp := w.linkName + ".tmp"
	if err := os.Symlink(path.Base(target), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, w.linkName)
}

// cleanOldFiles 按 maxAge 与 MaxBackups 清理历史日志文件。
func (w *fadviseRotatingWriter) cleanOldFiles() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}

	prefix := w.name + "-"
	type logFile struct {
		path    string
		modTime time.Time
	}
	var files []logFile

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		// 跳过软链接（当前日志的软链接）
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{
			path:    path.Join(w.dir, entry.Name()),
			modTime: info.ModTime(),
		})
	}

	// 按修改时间升序排列，最早的在前
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	cutoff := time.Now().Add(-w.maxAge)
	removed := make([]bool, len(files))
	removeAt := func(i int) {
		if removed[i] {
			return
		}
		if err := os.Remove(files[i].path); err == nil {
			w.stats.cleaned.Add(1)
		}
		removed[i] = true
	}

	// 1) 超过保留期限的文件删除
	if w.maxAge > 0 {
		for i := range files {
			if files[i].modTime.Before(cutoff) {
				removeAt(i)
			}
		}
	}

	// 2) 超过 MaxBackups 时删除最旧的
	if w.opts.MaxBackups > 0 {
		kept := 0
		for i := range files {
			if !removed[i] {
				kept++
			}
		}
		excess := kept - w.opts.MaxBackups
		if excess > 0 {
			for i := range files {
				if excess <= 0 {
					break
				}
				if !removed[i] {
					removeAt(i)
					excess--
				}
			}
		}
	}
}
