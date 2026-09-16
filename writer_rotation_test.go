package zlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a race free fake clock: the writer's background flusher reads
// the same clock the test advances.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(t time.Time) *testClock {
	return &testClock{now: t}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *testClock) Advance(d time.Duration) {
	c.Set(c.Now().Add(d))
}

// Hourly rotation must keep every entry in the file named after the hour it
// was written in, including the entries written across a day boundary.
func TestFileWriterKeepsEntriesInMatchingHourFile(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 15, 22, 59, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	want := map[string][]string{}
	for i := 0; i < 4*60; i++ { // four hours of one entry per minute
		clock.Advance(time.Minute)
		line := clock.Now().Format("2006-01-02-15-04") + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		hour := clock.Now().Format("2006-01-02-15")
		want[hour] = append(want[hour], line)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	t.Logf("files: %v", got)

	for hour, lines := range want {
		b, err := os.ReadFile(filepath.Join(dir, "app-"+hour+".log"))
		if err != nil {
			t.Fatalf("hour %s: %v", hour, err)
		}
		if string(b) != strings.Join(lines, "") {
			t.Fatalf("hour %s holds %d entries, want %d\ngot tail: %q",
				hour, strings.Count(string(b), "\n"), len(lines), lastLines(string(b), 3))
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// A restart right after midnight must not lose entries and must not write
// into the previous day's file.
func TestFileWriterRestartAfterMidnight(t *testing.T) {
	dir := t.TempDir()

	before := newTestClock(time.Date(2026, 9, 15, 23, 55, 0, 0, time.Local))
	w1, err := newFileWriter(writerConfig{dir: dir, name: "app"}, before.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Write([]byte("before-midnight\n")); err != nil {
		t.Fatal(err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	after := newTestClock(time.Date(2026, 9, 16, 0, 5, 0, 0, time.Local))
	w2, err := newFileWriter(writerConfig{dir: dir, name: "app"}, after.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte("after-midnight\n")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(dir, "app-2026-09-16-00.log")); got != "after-midnight\n" {
		t.Fatalf("post-restart file: %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "app-2026-09-15-23.log")); got != "before-midnight\n" {
		t.Fatalf("pre-midnight file: %q", got)
	}
}

// A clock stepped backwards (NTP on a board without an RTC) must not leave
// the writer appending to a file whose name no longer matches the clock.
func TestFileWriterClockSteppedBackwards(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 16, 0, 30, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	write := func(s string) {
		t.Helper()
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
	}

	write("at-0030\n")
	clock.Set(time.Date(2026, 9, 15, 21, 10, 0, 0, time.Local))
	write("after-step-back\n")

	if got := readFile(t, filepath.Join(dir, "app-2026-09-15-21.log")); got != "after-step-back\n" {
		t.Fatalf("entry after a backward clock step went to the wrong file: %q", got)
	}
}

// A failing write (a full disk, for example) must not silently discard
// buffered entries.
func TestFileWriterKeepsBufferedDataOnWriteError(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 16, 1, 0, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.mu.Lock()
	path := w.file.Name()
	good := w.file
	w.mu.Unlock()

	// a read-only handle makes every write fail, like ENOSPC would
	ro, err := os.OpenFile(path, os.O_RDONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	w.mu.Lock()
	w.file = ro
	w.mu.Unlock()

	if _, err := w.Write([]byte("must-survive\n")); err != nil {
		t.Fatalf("buffering an entry must not fail: %v", err)
	}
	if err := w.Sync(); err == nil {
		t.Fatal("expected the flush to fail against a read-only handle")
	}

	w.mu.Lock()
	w.file = good
	w.mu.Unlock()

	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, "must-survive\n") {
		t.Fatalf("buffered entry was dropped after a write error: %q", got)
	}
}

// A rotation failure must not drop the entry that triggered it.
func TestFileWriterRotationFailureKeepsEntries(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 16, 0, 59, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// break the log path so the next rotation cannot create the new file
	moved := dir + "_moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}

	clock.Set(time.Date(2026, 9, 16, 1, 0, 30, 0, time.Local))
	if _, err := w.Write([]byte("during-failed-rotation\n")); err != nil {
		t.Fatalf("write must keep flowing while rotation is broken: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("sync must keep working while rotation is broken: %v", err)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Date(2026, 9, 16, 1, 5, 0, 0, time.Local))
	if _, err := w.Write([]byte("after-recovery\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var all string
	for _, f := range files {
		all += readFile(t, f)
	}
	for _, want := range []string{"during-failed-rotation\n", "after-recovery\n"} {
		if !strings.Contains(all, want) {
			t.Fatalf("entry %q was lost across a failed rotation; files hold %q", want, all)
		}
	}
}

// The live log link must resolve even when Path is relative (go-zero's
// default), otherwise tailing it silently fails.
func TestFileWriterLinkResolves(t *testing.T) {
	base := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	clock := newTestClock(time.Date(2026, 9, 16, 2, 0, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: filepath.Join("logs", "app"), name: "app"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("live\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if _, err := w.Write([]byte("rotated\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join("logs", "app", "app.log")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(target) {
		t.Fatalf("link target %q must be absolute", target)
	}
	if got := readFile(t, link); got != "rotated\n" {
		t.Fatalf("reading through the live link returned %q", got)
	}
}

// Retention must not delete a sibling logger's files that merely share a
// name prefix ("app" vs "app-errors" writing into the same directory).
func TestFileWriterCleanupLeavesSiblingFilesAlone(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.Local)
	own := filepath.Join(dir, "app-2026-08-18-10.log")
	sibling := filepath.Join(dir, "app-errors-2026-08-18-10.log")
	for _, p := range []string{own, sibling} {
		if err := os.WriteFile(p, []byte("x"), fileMode); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-720 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	w, err := newFileWriter(writerConfig{dir: dir, name: "app", keepDays: 1}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("expired file of this logger should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling logger file must be left alone: %v", err)
	}
}

// MaxBackups (cfg.maxFiles) caps how many log files the directory holds:
// the newest N files survive, the file being written is always one of them.
func TestFileWriterMaxFilesKeepsNewestHourlyFiles(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 16, 0, 10, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app", maxFiles: 4}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 9; i++ {
		if i > 0 {
			clock.Advance(time.Hour)
		}
		line := fmt.Sprintf("hour-%02d\n", i)
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
	}

	got := ownLogFiles(t, dir, "app")
	want := []string{
		"app-2026-09-16-05.log",
		"app-2026-09-16-06.log",
		"app-2026-09-16-07.log",
		"app-2026-09-16-08.log",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("retained files = %v, want %v", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "app-2026-09-16-08.log")); got != "hour-08\n" {
		t.Fatalf("current file content = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "app-2026-09-16-05.log")); got != "hour-05\n" {
		t.Fatalf("oldest retained file content = %q", got)
	}
}

// maxFiles = 1 keeps only the file being written; 0 keeps everything, which
// is go-zero's default and the previous behaviour of this library.
func TestFileWriterMaxFilesEdges(t *testing.T) {
	for _, tc := range []struct {
		maxFiles int
		want     int
	}{
		{maxFiles: 1, want: 1},
		{maxFiles: 2, want: 2},
		{maxFiles: 0, want: 4},
	} {
		t.Run(fmt.Sprintf("maxFiles=%d", tc.maxFiles), func(t *testing.T) {
			dir := t.TempDir()
			clock := newTestClock(time.Date(2026, 9, 16, 0, 10, 0, 0, time.Local))
			w, err := newFileWriter(writerConfig{dir: dir, name: "app", maxFiles: tc.maxFiles}, clock.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()

			for i := 0; i < 4; i++ {
				if i > 0 {
					clock.Advance(time.Hour)
				}
				if _, err := w.Write([]byte("entry\n")); err != nil {
					t.Fatal(err)
				}
				if err := w.Sync(); err != nil {
					t.Fatal(err)
				}
			}

			files := ownLogFiles(t, dir, "app")
			if len(files) != tc.want {
				t.Fatalf("retained %d files (%v), want %d", len(files), files, tc.want)
			}
			current := "app-" + clock.Now().Format("2006-01-02-15") + ".log"
			if _, err := os.Stat(filepath.Join(dir, current)); err != nil {
				t.Fatalf("the file being written must never be removed: %v", err)
			}
		})
	}
}

// Retention by file count must not touch a sibling logger's files either.
func TestFileWriterMaxFilesLeavesSiblingFilesAlone(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "app-errors-2026-09-16-00.log")
	if err := os.WriteFile(sibling, []byte("keep me\n"), fileMode); err != nil {
		t.Fatal(err)
	}

	clock := newTestClock(time.Date(2026, 9, 16, 1, 10, 0, 0, time.Local))
	w, err := newFileWriter(writerConfig{dir: dir, name: "app", maxFiles: 1}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("own\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, sibling); got != "keep me\n" {
		t.Fatalf("sibling logger file was modified: %q", got)
	}
}

// KeepDays and MaxBackups are independent: a file goes away when either rule
// is violated, so the effective window is the stricter of the two.
func TestFileWriterKeepDaysAndMaxFilesCombined(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	setup := func(t *testing.T) string {
		dir := t.TempDir()
		for _, hour := range []string{"06", "07", "08", "09"} {
			p := filepath.Join(dir, "app-2026-09-16-"+hour+".log")
			if err := os.WriteFile(p, []byte("x\n"), fileMode); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-48 * time.Hour)
		stale := filepath.Join(dir, "app-2026-09-14-06.log")
		if err := os.WriteFile(stale, []byte("x\n"), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(stale, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("KeepDays binds", func(t *testing.T) {
		dir := setup(t)
		// a large MaxBackups does not protect a file past KeepDays
		w, err := newFileWriter(writerConfig{
			dir: dir, name: "app", keepDays: 1, maxFiles: 100,
		}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		if _, err := os.Stat(filepath.Join(dir, "app-2026-09-14-06.log")); !os.IsNotExist(err) {
			t.Fatalf("file past KeepDays must be removed, stat err = %v", err)
		}
		if got := ownLogFiles(t, dir, "app"); len(got) != 5 { // 4 kept + current
			t.Fatalf("retained %v, want 5 files", got)
		}
	})

	t.Run("MaxBackups binds", func(t *testing.T) {
		dir := setup(t)
		// a large KeepDays does not protect files beyond MaxBackups
		w, err := newFileWriter(writerConfig{
			dir: dir, name: "app", keepDays: 30, maxFiles: 2,
		}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		got := ownLogFiles(t, dir, "app")
		want := []string{"app-2026-09-16-09.log", "app-2026-09-16-10.log"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("retained %v, want %v", got, want)
		}
	})
}

func ownLogFiles(t *testing.T, dir, name string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, name+"-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
