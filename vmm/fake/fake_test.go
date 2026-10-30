package fake_test

import (
	"testing"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/vmmtest"
)

func spec(id string) vmm.Spec {
	return vmm.Spec{ID: id, Kernel: "/guest/vmlinux", Initrd: "/guest/initrd", VCPUs: 1, MemoryMiB: 512,
		Shares: []vmm.Share{{Tag: "volumes", HostPath: "/srv/herd/volumes/" + id}}}
}

func TestContractMemoryHost(t *testing.T) {
	var store *fake.Store
	vmmtest.Run(t, vmmtest.Harness{
		New: func(t *testing.T) vmm.VMM {
			store = fake.NewStore()
			return fake.New(vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64",
				SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-tigerlake"}, store)
		},
		Elsewhere: func(t *testing.T) vmm.VMM {
			return fake.New(vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64",
				SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-lunarlake"}, store)
		},
		Spec: spec,
	})
}

func TestContractVolumesHost(t *testing.T) {
	var store *fake.Store
	vmmtest.Run(t, vmmtest.Harness{
		New: func(t *testing.T) vmm.VMM {
			store = fake.NewStore()
			return fake.New(vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64",
				SuspendMaxFidelity: vmm.FidelityVolumes}, store)
		},
		Elsewhere: func(t *testing.T) vmm.VMM {
			return fake.New(vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "arm64",
				SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-arm64-neoverse-n1"}, store)
		},
		Spec: spec,
	})
}
