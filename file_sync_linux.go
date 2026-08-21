//go:build linux && (amd64 || arm64)

package zlog

import (
	"os"
	"syscall"
)

const fadvDontNeed = 4 // POSIX_FADV_DONTNEED

// syncAndDrop flushes the file's data to disk and evicts the file's pages
// from the kernel page cache. Log files are append-only and never re-read,
// so after writeback the cached pages are pure waste: without this hint a
// busy log keeps growing the cgroup's page cache (visible as inflated
// Memory in systemd/systemd-oomd). It is a hint, errors are ignored.
func syncAndDrop(f *os.File) {
	fd := f.Fd()
	_, _, _ = syscall.Syscall(syscall.SYS_FDATASYNC, fd, 0, 0)
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, fd, 0, 0, fadvDontNeed, 0, 0)
}
