package cloudhypervisor

import (
	"slices"
	"strings"
	"testing"
)

// TestVirtiofsdCacheAuto: herd starts virtiofsd with --cache=auto (ADR A-52). Under --cache=never the guest's FUSE
// client opens files for direct I/O and refuses a shared mmap, so a JVM fails to start from jars on /w (PLT-379). The
// operator's -virtiofsd-args still come last.
func TestVirtiofsdCacheAuto(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		extra := []string{"--sandbox=none", "--seccomp=none"}
		args := VirtiofsdArgs("/s.sock", "/d", readOnly, extra)
		var modes []string
		for _, a := range args {
			if strings.HasPrefix(a, "--cache=") {
				modes = append(modes, a)
			}
		}
		if want := []string{"--cache=auto"}; !slices.Equal(modes, want) {
			t.Errorf("VirtiofsdArgs(readOnly=%v) = %q: cache options %q, want %q", readOnly, args, modes, want)
		}
		if tail := args[len(args)-len(extra):]; !slices.Equal(tail, extra) {
			t.Errorf("VirtiofsdArgs(readOnly=%v) = %q, want the extra arguments %q last", readOnly, args, extra)
		}
	}
}
