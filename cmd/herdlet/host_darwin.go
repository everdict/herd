//go:build darwin && arm64 && cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/vmm/vz"
)

// newHost is Virtualization.framework (ADR R-D8): native vsock, virtio-fs, VOLUMES unless the Mac passed the
// save/restore probe.
func newHost(ctx context.Context, o options) (Host, error) {
	host := o.vzHost
	if host == "" {
		h, err := os.Hostname()
		if err != nil {
			return Host{}, err
		}
		host = strings.ToLower(strings.Split(h, ".")[0])
	}
	v, err := vz.New(vz.Config{StateDir: filepath.Join(o.state, "vms"), SnapshotDir: filepath.Join(o.state, "vm-snapshots"), Host: host,
		Probe: vz.ProbeConfig{Kernel: o.kernel, Initrd: o.initrd, Cmdline: o.cmdline}}, vz.Native())
	if err != nil {
		return Host{}, err
	}
	return Host{VMM: v, Guests: v, Version: "virtualization.framework"}, nil
}
