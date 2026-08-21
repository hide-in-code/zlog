package zlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// defaultBufferSize caps the in-memory buffer per logger; producers
	// block (lossless) instead of dropping entries when it is full.
	defaultBufferSize = 1 << 20 // 1MB
	// flushInterval is how often buffered entries are written to the file.
	flushInterval = 100 * time.Millisecond
	// syncInterval is how often data is fsync'd and evicted from the page
	// cache. It bounds both the crash loss window and cache memory growth.
	syncInterval = 1 * time.Second
	// syncBytesThreshold triggers an early sync+fadvise when this many
	// dirty bytes accumulate, bounding cache growth on write bursts.
	syncBytesThreshold = 64 << 20 // 64MB
	// defaultMaxSizeMB is used when Rotation=size but MaxSize is unset.
	defaultMaxSizeMB = 100
	// cleanupInterval is the minimum interval between directory scans
	// that enforce KeepDays/MaxBackups.
	cleanupInterval = time.Hour

	fileMode = 0o644
	dirMode  = 0o755
)

type writerConfig struct {
	dir        string
	name       string
	keepDays   int
	maxBackups int
	maxSize    int64 // bytes; only honored when rotation == "size"
	rotation   string
}

// fileWriter is a concurrency-safe, buffered, rotating file writer.
// It is a zapcore.WriteSyncer. Every syncInterval (or syncBytesThreshold
// bytes) it fsyncs the file and drops the file's pages from the kernel
// page cache, which is what keeps the process cgroup memory from being
// inflated by log volume.
type fileWriter struct {
	cfg writerConfig

	mu          sync.Mutex
	buf         []byte
	file        *os.File
	curLen      int64 // bytes in the current file
	dirty       int64 // bytes written since the last sync+fadvise
	lastSync    time.Time
	lastCleanup time.Time
	nextRotate  time.Time
	closed      bool
	now         func() time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	errOnce sync.Once // report the first internal failure to stderr only
}

func newFileWriter(cfg writerConfig, now ...func() time.Time) (*fileWriter, error) {
	if cfg.rotation == "size" && cfg.maxSize <= 0 {
		cfg.maxSize = int64(defaultMaxSizeMB) << 20
	}
	clock := time.Now
	if len(now) > 0 && now[0] != nil {
		clock = now[0]
	}
	w := &fileWriter{
		cfg:         cfg,
		buf:         make([]byte, 0, defaultBufferSize),
		now:         clock,
		lastCleanup: clock(),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	w.cleanupLocked()
	go w.loop()
	return w, nil
}

func (w *fileWriter) loop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.mu.Lock()
			_ = w.flushLocked()
			if w.now().Sub(w.lastSync) >= syncInterval {
				_ = w.syncLocked()
			}
			if w.now().Sub(w.lastCleanup) >= cleanupInterval {
				w.lastCleanup = w.now()
				w.cleanupLocked()
			}
			w.mu.Unlock()
		}
	}
}

func (w *fileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if len(p) > cap(w.buf) {
		// oversized entry: bypass the buffer, but drain pending data first
		if err := w.flushLocked(); err != nil {
			return 0, err
		}
		return w.writeFileLocked(p)
	}
	if len(w.buf)+len(p) > cap(w.buf) {
		if err := w.flushLocked(); err != nil {
			return 0, err
		}
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *fileWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.flushLocked(); err != nil {
		return err
	}
	return w.syncLocked()
}

func (w *fileWriter) Close() error {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.flushLocked()
	_ = w.syncLocked()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

func (w *fileWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.writeFileLocked(w.buf)
	w.buf = w.buf[:0]
	return err
}

func (w *fileWriter) writeFileLocked(p []byte) (int, error) {
	if w.file == nil {
		// self-heal: a previous failed rotation must not kill the logger
		if err := w.open(); err != nil {
			w.reportErr(err)
			return 0, err
		}
	}
	if w.shouldRotateLocked() {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.curLen += int64(n)
	w.dirty += int64(n)
	if err != nil {
		return n, err
	}
	if w.cfg.rotation == "size" && w.cfg.maxSize > 0 && w.curLen >= w.cfg.maxSize {
		if rerr := w.rotateLocked(); rerr != nil {
			return n, rerr
		}
	}
	if w.dirty >= syncBytesThreshold {
		if serr := w.syncLocked(); serr != nil {
			return n, serr
		}
	}
	return n, nil
}

func (w *fileWriter) shouldRotateLocked() bool {
	return w.cfg.rotation != "size" && w.now().After(w.nextRotate)
}

func (w *fileWriter) rotateLocked() error {
	if w.file == nil {
		return os.ErrClosed
	}
	// flush data out and drop the old file's page cache before switching
	_ = w.syncLocked()
	next, err := w.openNext()
	if err != nil {
		// keep writing to the old file instead of going dead; the caller
		// retries rotation on the next write once the path recovers
		w.reportErr(err)
		return err
	}
	old := w.file
	w.file = next
	w.curLen = 0
	w.dirty = 0
	w.lastSync = w.now()
	w.nextRotate = w.now().Truncate(time.Hour).Add(time.Hour)
	_ = old.Close()
	w.updateLink()
	w.cleanupLocked()
	return nil
}

// reportErr surfaces the first internal failure (e.g. ENOSPC) on stderr;
// zap itself also reports write errors to its ErrorOutput on every entry,
// so this only covers writer-internal failures like a failed rotation.
func (w *fileWriter) reportErr(err error) {
	w.errOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[zlog] %s: %v (further errors suppressed)\n", w.cfg.name, err)
	})
}

func (w *fileWriter) syncLocked() error {
	if w.file == nil || w.dirty == 0 {
		return nil
	}
	syncAndDrop(w.file)
	w.dirty = 0
	w.lastSync = w.now()
	return nil
}

func (w *fileWriter) open() error {
	if err := os.MkdirAll(w.cfg.dir, dirMode); err != nil {
		return err
	}
	f, err := w.openNext()
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.curLen = st.Size()
	w.dirty = 0
	w.lastSync = w.now()
	w.nextRotate = w.now().Truncate(time.Hour).Add(time.Hour)
	// evict any stale page cache left by a previous process
	if st.Size() > 0 {
		syncAndDrop(f)
	}
	w.updateLink()
	return nil
}

// openNext opens the file that should be written to next (current rotation
// slot). It does not touch writer state, so a failure leaves the writer
// fully usable.
func (w *fileWriter) openNext() (*os.File, error) {
	return os.OpenFile(filepath.Join(w.cfg.dir, w.fileName(w.now())),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
}

// fileName returns the current file name: hourly when rotation is
// time-based (backward compatible), second-granularity when size-based.
func (w *fileWriter) fileName(t time.Time) string {
	if w.cfg.rotation == "size" {
		return w.uniqueName(fmt.Sprintf("%s-%s.log", w.cfg.name, t.Format("2006-01-02-15-04-05")))
	}
	return fmt.Sprintf("%s-%s.log", w.cfg.name, t.Format("2006-01-02-15"))
}

func (w *fileWriter) uniqueName(base string) string {
	if _, err := os.Stat(filepath.Join(w.cfg.dir, base)); os.IsNotExist(err) {
		return base
	}
	trimmed := strings.TrimSuffix(base, ".log")
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d.log", trimmed, i)
		if _, err := os.Stat(filepath.Join(w.cfg.dir, cand)); os.IsNotExist(err) {
			return cand
		}
	}
}

// updateLink repoints <name>.log to the current file so tail/readers
// always follow the live log.
func (w *fileWriter) updateLink() {
	link := filepath.Join(w.cfg.dir, w.cfg.name+".log")
	_ = os.Remove(link)
	_ = os.Symlink(w.file.Name(), link)
}

// cleanupLocked enforces KeepDays (by mtime) and, for size rotation,
// MaxBackups (newest N rotated files, current file excluded).
func (w *fileWriter) cleanupLocked() {
	entries, err := os.ReadDir(w.cfg.dir)
	if err != nil {
		return
	}
	prefix := w.cfg.name + "-"
	current := ""
	if w.file != nil {
		current = filepath.Base(w.file.Name())
	}
	now := w.now()
	var backups []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if e.Name() == current {
			continue
		}
		backups = append(backups, e.Name())
		if w.cfg.keepDays > 0 {
			if info, ierr := e.Info(); ierr == nil &&
				now.Sub(info.ModTime()) > time.Duration(w.cfg.keepDays)*24*time.Hour {
				_ = os.Remove(filepath.Join(w.cfg.dir, e.Name()))
			}
		}
	}
	if w.cfg.rotation == "size" && w.cfg.maxBackups > 0 && len(backups) > w.cfg.maxBackups {
		sort.Sort(sort.Reverse(sort.StringSlice(backups))) // names sort by time, newest first
		for _, b := range backups[w.cfg.maxBackups:] {
			_ = os.Remove(filepath.Join(w.cfg.dir, b))
		}
	}
}
