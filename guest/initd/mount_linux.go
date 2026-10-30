//go:build linux

package initd

import "syscall"

// KernelMounter mounts with mount(2).
type KernelMounter struct{}

func (KernelMounter) Mount(source, target, fstype string, flags uintptr, data string) error {
	return syscall.Mount(source, target, fstype, flags, data)
}
