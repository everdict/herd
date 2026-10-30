//go:build !linux

package agent

import (
	"os"
	"syscall"
)

func syncAll() { _ = syscall.Sync() }

// syncFS has no syncfs(2) outside linux: the directory's own fsync is the closest, after syncAll.
func syncFS(f *os.File) error { return f.Sync() }
