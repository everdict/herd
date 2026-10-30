//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"syscall"

	"github.com/everdict/herd/guest/transport"
)

func run() error {
	for _, m := range []struct{ src, dst, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"herd", share, "virtiofs"},
	} {
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.src, m.dst, m.fstype, 0, ""); err != nil && err != syscall.EBUSY {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.dst, err)
		}
	}
	l, err := transport.ListenVsock(port)
	if err != nil {
		return err
	}
	log.Printf("vzguest: serving on vsock port %d", port)
	return serve(l)
}
