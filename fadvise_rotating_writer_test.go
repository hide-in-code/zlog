package zlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
)

func newTestWriter(t *testing.T, dir, name string, maxAge time.Duration, mutate func(*Options)) *fadviseRotatingWriter {
	t.Helper()
	opts := defaultOptions()
	if mutate != nil {
		mutate(&opts)
	}
	return newFadviseRotatingWriter(dir, name, maxAge, opts)
}

// countLogFiles 统计目录下真实日志文件数（排除软链接）。
func countLogFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		n++
	}
	return n
}

func TestWriterWritesAndLinks(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = false
	})
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "app.log")
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("content mismatch: %q", data)
	}
}

func TestFadviseThrottleByInterval(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = true
		o.FadviseInterval = 100 * time.Millisecond
		o.FadviseMinBytes = 0 // 只测时间间隔这一路，字节阈值置 0
		o.SyncBeforeFadvise = false
	})
	defer w.Close()

	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.Stats().FadviseCalls; got != 1 {
		t.Fatalf("within interval: expected 1 fadvise, got %d", got)
	}

	time.Sleep(120 * time.Millisecond)
	if _, err := w.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().FadviseCalls; got != 2 {
		t.Fatalf("after interval: expected 2 fadvises, got %d", got)
	}
}

func TestFadviseThrottleByBytes(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = true
		o.FadviseInterval = 0
		o.FadviseMinBytes = 100
		o.SyncBeforeFadvise = false
	})
	defer w.Close()

	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(strings.Repeat("a", 10))); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.Stats().FadviseCalls; got != 0 {
		t.Fatalf("below min bytes: expected 0 fadvises, got %d", got)
	}

	if _, err := w.Write([]byte(strings.Repeat("b", 200))); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().FadviseCalls; got != 1 {
		t.Fatalf("above min bytes: expected 1 fadvise, got %d", got)
	}
	if got := w.Stats().FadviseErrs; got != 0 {
		t.Fatalf("unexpected fadvise errors: %d", got)
	}
}

func TestSizeRotation(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = false
	})
	defer w.Close()
	w.maxSize = 10 // 直接以字节为单位覆盖阈值，便于测试

	big := []byte(strings.Repeat("x", 8))
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(big); err != nil { // 16 > 10，触发大小轮转
		t.Fatal(err)
	}

	if n := countLogFiles(t, dir); n != 2 {
		t.Fatalf("expected 2 log files, got %d", n)
	}
	link, err := os.Readlink(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(link, "-1.log") {
		t.Fatalf("symlink should point to newest seq file, got %s", link)
	}
	if got := w.Stats().Rotations; got != 2 { // 初始化 1 次 + 大小轮转 1 次
		t.Fatalf("expected 2 rotations, got %d", got)
	}
}

func TestTimeRotation(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = false
		o.RotationInterval = 20 * time.Millisecond
	})
	defer w.Close()

	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := w.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}

	if n := countLogFiles(t, dir); n != 2 {
		t.Fatalf("expected 2 log files after time rotation, got %d", n)
	}
}

func TestCleanupByAge(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for i := 1; i <= 4; i++ {
		f := filepath.Join(dir, fmt.Sprintf("app-2026-01-0%d-00.log", i))
		if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	w := newTestWriter(t, dir, "app", 24*time.Hour, func(o *Options) {
		o.FadviseEnabled = false
	})
	defer w.Close()

	// 4 个过期文件被清掉，只剩新建的当前文件
	if n := countLogFiles(t, dir); n != 1 {
		t.Fatalf("expected 1 file after age cleanup, got %d", n)
	}
	if got := w.Stats().CleanedFiles; got != 4 {
		t.Fatalf("expected 4 cleaned files, got %d", got)
	}
}

func TestMaxBackups(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = false
		o.RotationInterval = 0
		o.MaxBackups = 2
	})
	defer w.Close()
	w.maxSize = 4

	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("abcd")); err != nil {
			t.Fatal(err)
		}
	}

	if n := countLogFiles(t, dir); n != 2 {
		t.Fatalf("expected 2 files kept by MaxBackups, got %d", n)
	}
}

func TestRelativePathSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("logs", 0755); err != nil {
		t.Fatal(err)
	}

	w := newTestWriter(t, "logs", "app", 0, func(o *Options) {
		o.FadviseEnabled = false
	})
	defer w.Close()

	if _, err := w.Write([]byte("data\n")); err != nil {
		t.Fatal(err)
	}

	// 相对路径目录下，软链接也必须可解析
	data, err := os.ReadFile(filepath.Join("logs", "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data\n" {
		t.Fatalf("content mismatch: %q", data)
	}
}

func TestFadviseHardCapOverridesInterval(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, "app", 0, func(o *Options) {
		o.FadviseEnabled = true
		o.FadviseInterval = time.Hour // 时间间隔极长，验证字节硬顶可绕过它
		o.FadviseMinBytes = 100
		o.SyncBeforeFadvise = false
	})
	defer w.Close()

	// 首次写入 250 字节：累计 250 >= 2*100，硬顶触发
	if _, err := w.Write([]byte(strings.Repeat("a", 250))); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().FadviseCalls; got != 1 {
		t.Fatalf("expected 1 fadvise, got %d", got)
	}

	// 间隔内再写 150 字节：累计 150 < 2*100，被 1h 时间间隔阻止
	if _, err := w.Write([]byte(strings.Repeat("b", 150))); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().FadviseCalls; got != 1 {
		t.Fatalf("expected no fadvise within interval, got %d", got)
	}

	// 再写 150 字节：累计 300 >= 2*100，硬顶无视时间间隔立即释放
	if _, err := w.Write([]byte(strings.Repeat("c", 150))); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().FadviseCalls; got != 2 {
		t.Fatalf("expected fadvise via hard cap, got %d", got)
	}
}

func TestZlogIntegration(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{
		Mode:     "file",
		Path:     dir,
		Encoding: "json",
		Level:    "debug",
	}
	l := NewWithConf("test", conf, WithFadviseMinBytes(1), WithFadviseInterval(0))
	defer CloseAll()

	l.Info("hello zlog", zap.String("k", "v"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "test", "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "hello zlog") || !strings.Contains(s, `"k":"v"`) ||
		!strings.Contains(s, `"module":"test"`) {
		t.Fatalf("log content incomplete: %s", s)
	}
	if st := l.Stats(); st.BytesWritten == 0 || st.Writes == 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}
