//go:build darwin

package deploy

import "golang.org/x/sys/unix"

// hostMemoryMiB is hw.memsize; 0 when it cannot be read.
func hostMemoryMiB() int64 {
	b, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(b >> 20)
}
