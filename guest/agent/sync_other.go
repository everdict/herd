//go:build !linux

package agent

import "os"

// syncFS has no syncfs(2) outside linux: the directory's own fsync is the closest.
func syncFS(f *os.File) error { return f.Sync() }
