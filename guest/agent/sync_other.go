//go:build !linux

package agent

import "os"

// syncFS has no syncfs(2) outside linux: the directory's own fsync is the closest.
func syncFS(f *os.File) error { return f.Sync() }

// dropCaches has no /proc/sys/vm/drop_caches outside linux.
func dropCaches() error { return nil }
