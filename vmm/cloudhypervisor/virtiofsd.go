package cloudhypervisor

import "strings"

// VirtiofsdArgs are the arguments herd starts virtiofsd with for one share: extra (the operator's -virtiofsd-args)
// come last. It is what launch passes and what `herdlet doctor` checks the given virtiofsd accepts, so the two cannot
// drift apart. --migration-mode needs virtiofsd >= 1.11 (Ubuntu 24.04's 1.10 rejects it).
func VirtiofsdArgs(socket, sharedDir string, readOnly bool, extra []string) []string {
	args := []string{"--socket-path=" + socket, "--shared-dir=" + sharedDir, "--cache=never",
		// Snapshots with virtio-fs: virtiofsd re-finds open files by path on restore (R0 spike).
		"--migration-mode=find-paths"}
	if readOnly {
		args = append(args, "--readonly")
	}
	return append(args, extra...)
}

// VirtiofsdFlags are the long options herd may pass to virtiofsd (VirtiofsdArgs for a read-only share), without their
// values, in order.
func VirtiofsdFlags(extra []string) []string {
	var flags []string
	for _, a := range VirtiofsdArgs("s", "d", true, extra) {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		flags = append(flags, name)
	}
	return flags
}
