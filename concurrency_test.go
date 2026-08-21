package zlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"go.uber.org/zap"
)

// TestConcurrentWriteIntegrity 用 16 个协程并发写入，验证：
// 1) 日志行数精确（无丢失）；2) 每行都是完整 JSON（无行间交错/撕裂）；
// 3) 无重复行。zap 编码完整行后一次性交给 WriteSyncer，
// BufferedWriteSyncer 与底层 writer 各自持锁，行内容不会被拆散。
func TestConcurrentWriteIntegrity(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{Mode: "file", Path: dir, Encoding: "json", Level: "debug"}
	l := NewWithConf("conc", conf, WithFadviseMinBytes(1), WithFadviseInterval(0))
	defer CloseAll()

	const writers = 16
	const perWriter = 500
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				l.Info("concurrent", zap.Int("goroutine", id), zap.Int("seq", j))
			}
		}(g)
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "conc", "conc.log"))
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("expected %d lines, got %d", writers*perWriter, len(lines))
	}

	seen := make(map[string]bool, len(lines))
	for _, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Fatalf("invalid JSON line (line corruption): %q", ln)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("unmarshal failed: %v line=%q", err, ln)
		}
		if m["msg"] != "concurrent" {
			t.Fatalf("unexpected msg: %q", ln)
		}
		key := fmt.Sprintf("%v/%v", m["goroutine"], m["seq"])
		if seen[key] {
			t.Fatalf("duplicate line: %s", key)
		}
		seen[key] = true
	}
}

// TestNoGoroutineLeak 反复创建/关闭 Logger，验证所有后台协程
// （BufferedWriteSyncer 的刷新循环）都会随 Close/CloseAll 退出。
func TestNoGoroutineLeak(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{Mode: "file", Path: dir, Encoding: "json", Level: "debug"}
	base := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		l := NewWithConf(fmt.Sprintf("leak%d", i), conf)
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 200; j++ {
					l.Info("x", zap.Int("j", j))
				}
			}()
		}
		wg.Wait()
		_ = l.Close()
	}
	CloseAll()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: base=%d now=%d", base, runtime.NumGoroutine())
}

// TestMemorySteadyState 验证稳态下持续打日志不会造成堆内存无界增长
// （排除编码路径的内存泄漏；缓冲/编码器的常驻分配在预热阶段完成）。
func TestMemorySteadyState(t *testing.T) {
	dir := t.TempDir()
	conf := logx.LogConf{Mode: "file", Path: dir, Encoding: "json", Level: "debug"}
	l := NewWithConf("mem", conf)
	defer CloseAll()

	payload := strings.Repeat("x", 200)
	logLine := func() { l.Info("steady", zap.String("payload", payload)) }

	// 预热：让缓冲、编码器、时间戳等常驻分配稳定
	for i := 0; i < 2000; i++ {
		logLine()
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < 2000; i++ {
		logLine()
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	// 容忍 1MB：2000 条日志后不应有明显净增长
	if growth > 1<<20 {
		t.Fatalf("heap grew %d bytes after steady-state logging", growth)
	}
}
