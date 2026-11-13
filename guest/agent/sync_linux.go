//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFS flushes the filesystem f is on and reports its write-back errors.
func syncFS(f *os.File) error { return unix.Syncfs(int(f.Fd())) }

// procDropCaches is the kernel's drop_caches knob; a test points it elsewhere.
var procDropCaches = "/proc/sys/vm/drop_caches"

// dropCaches frees the kernel's clean page cache and its reclaimable dentries and inodes (DropCachesLevel).
func dropCaches() error { return os.WriteFile(procDropCaches, []byte(DropCachesLevel), 0) }
