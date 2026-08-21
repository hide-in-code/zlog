package zlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestFileWriterWriteSyncClose(t *testing.T) {
	dir := t.TempDir()
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected content %q", data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileWriterSizeRotationAndByteIntegrity(t *testing.T) {
	dir := t.TempDir()
	w, err := newFileWriter(writerConfig{
		dir:      dir,
		name:     "app",
		rotation: "size",
		maxSize:  1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(strings.Repeat("x", 300) + "\n")
	total := 0
	for i := 0; i < 19; i++ { // 19*304=5776B -> 4 rotations, current file non-empty
		n, err := w.Write(msg)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("expected rotation, got %d files", len(files))
	}
	var sum int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		sum += st.Size()
	}
	if sum != int64(total) {
		t.Fatalf("byte loss: files total %d, written %d", sum, total)
	}
	link, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(link) == 0 {
		t.Fatal("live log link is empty")
	}
}

func TestFileWriterMaxBackups(t *testing.T) {
	dir := t.TempDir()
	w, err := newFileWriter(writerConfig{
		dir:        dir,
		name:       "app",
		rotation:   "size",
		maxSize:    128,
		maxBackups: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(strings.Repeat("y", 200) + "\n")
	for i := 0; i < 20; i++ {
		if _, err := w.Write(msg); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > 3 { // 2 backups + current
		t.Fatalf("expected at most 3 files, got %d", len(files))
	}
}

func TestFileWriterTimeRotation(t *testing.T) {
	dir := t.TempDir()
	cur := time.Date(2026, 8, 21, 14, 30, 0, 0, time.Local)
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"}, func() time.Time { return cur })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	cur = cur.Add(45 * time.Minute) // 15:15, crosses the hour boundary
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	first, err := os.ReadFile(filepath.Join(dir, "app-2026-08-21-14.log"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "app-2026-08-21-15.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "first\n" || string(second) != "second\n" {
		t.Fatalf("unexpected contents %q / %q", first, second)
	}
	link, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(link) != "second\n" {
		t.Fatalf("live link should point to newest file, got %q", link)
	}
}

func TestFileWriterKeepDaysCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.Local)
	old := filepath.Join(dir, "app-2026-08-18-10.log")
	fresh := filepath.Join(dir, "app-2026-08-21-10.log")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldT := now.Add(-72 * time.Hour)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}

	w, err := newFileWriter(writerConfig{dir: dir, name: "app", keepDays: 1}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired file should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh file should remain, stat err = %v", err)
	}
}

func TestFileWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 8
	const perG = 500
	msg := []byte("0123456789abcdef\n")

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := w.Write(msg); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		sum += st.Size()
	}
	if want := int64(goroutines * perG * len(msg)); sum != want {
		t.Fatalf("total bytes %d, want %d", sum, want)
	}
}

func TestFileWriterSurvivesRotationFailure(t *testing.T) {
	dir := t.TempDir()
	w, err := newFileWriter(writerConfig{dir: dir, name: "app", rotation: "size", maxSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	// break the log path so the next rotation cannot create a new file
	moved := dir + "_moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}

	// rotation now fails, but writes must keep flowing to the old fd
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte("data\n")); err != nil {
			t.Fatalf("write failed while rotation broken: %v", err)
		}
		if err := w.Sync(); err != nil {
			t.Fatalf("sync failed while rotation broken: %v", err)
		}
	}

	// restore the path; the writer must self-heal and rotate normally
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		sum += st.Size()
	}
	if sum < int64(len("data\n")*10+len("after\n")) {
		t.Fatalf("data lost across rotation failure: %d bytes on disk", sum)
	}
}

func TestSyncAndDrop(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	syncAndDrop(f) // must not panic on any platform
}

func TestLoggerIntegration(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{Mode: "file", Path: dir, Level: "info", Encoding: "json"}
	l := NewWithConf("svc", conf)
	l.Info("hello world", zap.Int("code", 42))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "svc", "svc.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"hello world", `"code":42`, `"module":"svc"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in log entry %q", want, s)
		}
	}
}

func TestLoggerSingletonAndCloseAll(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{Mode: "file", Path: dir}
	a := NewWithConf("dup", conf)
	b := NewWithConf("dup", conf)
	if a != b {
		t.Fatal("expected the same logger instance for the same name")
	}
	CloseAll()
	if len(_loggers) != 0 {
		t.Fatal("logger map should be empty after CloseAll")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]zapcore.Level{
		"debug":   zapcore.DebugLevel,
		"info":    zapcore.InfoLevel,
		"warn":    zapcore.WarnLevel,
		"warning": zapcore.WarnLevel,
		"error":   zapcore.ErrorLevel,
		"severe":  zapcore.ErrorLevel,
		"fatal":   zapcore.ErrorLevel,
		"":        zapcore.InfoLevel,
		"bogus":   zapcore.InfoLevel,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
