//go:build linux

package cloudhypervisor

// HostCPU is this host's processor.
func HostCPU() CPU { return ARM64CPU{} }
