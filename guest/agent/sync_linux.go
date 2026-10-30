//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncAll() { unix.Sync() }

// syncFS flushes the filesystem f is on and reports its write-back errors.
func syncFS(f *os.File) error { return unix.Syncfs(int(f.Fd())) }
