//go:build linux

package cloudhypervisor

func cpuid(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)

// HostCPU is this host's processor.
func HostCPU() CPU { return X86CPU{CPUID: cpuid} }
