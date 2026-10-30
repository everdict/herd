//go:build !linux

package initd

import (
	"fmt"
	"runtime"
)

// KernelMounter is available on linux only, where herd-guest boots.
type KernelMounter struct{}

func (KernelMounter) Mount(source, target, fstype string, flags uintptr, data string) error {
	return fmt.Errorf("initd: mount is not available on %s", runtime.GOOS)
}
