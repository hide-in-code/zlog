//go:build !linux || !(amd64 || arm64)

package zlog

import "os"

// syncAndDrop falls back to a plain fsync on platforms without an
// fadvise(FADV_DONTNEED) syscall; logs are still durable, only the page
// cache eviction is skipped.
func syncAndDrop(f *os.File) {
	_ = f.Sync()
}
