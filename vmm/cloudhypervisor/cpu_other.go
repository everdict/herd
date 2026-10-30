//go:build linux && !amd64 && !arm64

package cloudhypervisor

import (
	"fmt"
	"runtime"
)

type unsupportedCPU struct{}

func (unsupportedCPU) Arch() string { return runtime.GOARCH }

func (unsupportedCPU) Class() (string, error) {
	return "", fmt.Errorf("cloud-hypervisor: no restore family on %s", runtime.GOARCH)
}

// HostCPU is this host's processor.
func HostCPU() CPU { return unsupportedCPU{} }
