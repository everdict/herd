package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/vmm/cloudhypervisor"
)

// newHost is Cloud Hypervisor (ADR R-D8): one cloud-hypervisor process per VM, virtiofsd for the share, hybrid vsock.
func newHost(ctx context.Context, o options) (Host, error) {
	v := cloudhypervisor.New(cloudhypervisor.Config{
		Binary: o.chBinary, Virtiofsd: o.virtiofsd, VirtiofsdArgs: strings.Fields(o.virtiofsdArgs),
		StateDir: filepath.Join(o.state, "vms"), SnapshotDir: filepath.Join(o.state, "vm-snapshots"),
	})
	return Host{VMM: v, Guests: v, Version: "cloud-hypervisor v54.0"}, nil
}
