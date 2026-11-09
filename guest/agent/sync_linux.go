//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFS flushes the filesystem f is on and reports its write-back errors.
func syncFS(f *os.File) error { return unix.Syncfs(int(f.Fd())) }

// dropCaches frees the kernel's reclaimable dentries and inodes ("2"), which includes the virtio-fs share's.
func dropCaches() error { return os.WriteFile("/proc/sys/vm/drop_caches", []byte("2"), 0) }
