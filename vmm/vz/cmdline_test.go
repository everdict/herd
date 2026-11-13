package vz

import (
	"testing"

	"github.com/everdict/herd/vmm"
)

// TestGuestClearsFreedPages (PLT-591): a VM boots with init_on_free=1 (vmm.ZeroFreedPages) on the default command line
// and on a given one, so its freed memory is zero pages in a snapshot; a given init_on_free is kept.
func TestGuestClearsFreedPages(t *testing.T) {
	for cmdline, want := range map[string]string{
		"":                            "console=hvc0 panic=-1 init_on_free=1",
		"console=hvc0":                "console=hvc0 init_on_free=1",
		"console=hvc0 init_on_free=0": "console=hvc0 init_on_free=0",
	} {
		if got := machineConfig(vmm.Spec{Cmdline: cmdline}).Cmdline; got != want {
			t.Errorf("spec cmdline %q boots with %q, want %q", cmdline, got, want)
		}
	}
}
