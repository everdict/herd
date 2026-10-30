package vmm_test

import (
	"errors"
	"testing"

	"github.com/everdict/herd/vmm"
)

func TestRestoreFamilies(t *testing.T) {
	ch, err := vmm.CloudHypervisorRestoreFamily("amd64", "tigerlake")
	if err != nil || ch != "microvm-ch-amd64-tigerlake" {
		t.Errorf("CloudHypervisorRestoreFamily = %q, %v", ch, err)
	}
	v, err := vmm.VZRestoreFamily("nahyun-imac")
	if err != nil || v != "microvm-vz-arm64-nahyun-imac" {
		t.Errorf("VZRestoreFamily = %q, %v", v, err)
	}
	if vmm.PortableRestoreFamily != "microvm-volumes" {
		t.Errorf("PortableRestoreFamily = %q", vmm.PortableRestoreFamily)
	}
	for _, bad := range [][2]string{{"", "x"}, {"amd64", ""}, {"riscv64", "x"}, {"amd64", "Tiger Lake"}} {
		if f, err := vmm.CloudHypervisorRestoreFamily(bad[0], bad[1]); !errors.Is(err, vmm.ErrInvalidSpec) {
			t.Errorf("CloudHypervisorRestoreFamily(%q, %q) = %q, %v; want ErrInvalidSpec", bad[0], bad[1], f, err)
		}
	}
	if f, err := vmm.VZRestoreFamily(""); !errors.Is(err, vmm.ErrInvalidSpec) {
		t.Errorf("VZRestoreFamily(\"\") = %q, %v; want ErrInvalidSpec", f, err)
	}
}

func TestNotImplementedErrorIsTyped(t *testing.T) {
	var err error = &vmm.NotImplementedError{Backend: "vz", Op: vmm.OpSnapshot}
	if !errors.Is(err, vmm.ErrNotImplemented) {
		t.Errorf("NotImplementedError is not ErrNotImplemented")
	}
	var ni *vmm.NotImplementedError
	if !errors.As(err, &ni) || ni.Op != vmm.OpSnapshot {
		t.Errorf("errors.As lost the op: %+v", ni)
	}
	if errors.Is(err, vmm.ErrNotFound) {
		t.Errorf("NotImplementedError matches ErrNotFound")
	}
}

// Fidelity numbers are Substrate's SnapshotFidelity numbers so the API layer can pass them through.
func TestFidelityMatchesSubstrate(t *testing.T) {
	if vmm.FidelityVolumes != 1 || vmm.FidelityMemory != 3 {
		t.Errorf("VOLUMES=%d MEMORY=%d, want Substrate's 1 and 3", vmm.FidelityVolumes, vmm.FidelityMemory)
	}
	if vmm.FidelityVolumes.String() != "VOLUMES" || vmm.FidelityMemory.String() != "MEMORY" {
		t.Errorf("fidelity names %s %s", vmm.FidelityVolumes, vmm.FidelityMemory)
	}
}
