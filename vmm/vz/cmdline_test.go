package vz

import (
	"testing"

	"github.com/everdict/herd/vmm"
)

// TestCmdlineIsTheSpecs (PLT-591): a VM boots with the default command line or the given one, and nothing is added to
// it. v0.1.24 added init_on_free=1, with which a guest holds its whole memory on the host; a spec carrying it does not
// validate (vmm.CheckCmdline). Virtualization.framework's balloon reports no free pages, so on this backend a guest's
// freed memory stays in its MEMORY snapshot.
func TestCmdlineIsTheSpecs(t *testing.T) {
	for cmdline, want := range map[string]string{
		"":             "console=hvc0 panic=-1",
		"console=hvc0": "console=hvc0",
	} {
		if got := machineConfig(vmm.Spec{Cmdline: cmdline}).Cmdline; got != want {
			t.Errorf("spec cmdline %q boots with %q, want %q", cmdline, got, want)
		}
	}
	if err := vmm.CheckCmdline("console=hvc0 init_on_free=1"); err == nil {
		t.Error("a command line with init_on_free=1 was accepted")
	}
}
