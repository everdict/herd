package deploy

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/gvstack"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm/vz"
	"google.golang.org/grpc"
)

// macHost is a Mac's backend on hypervisor hv (ADR A-21, A-22): the vz VMM, whose save/restore probe boots this
// host's guest and whose fidelity follows the console session s, and herd-egress in-process behind one
// gvisor-tap-vsock stack per VM (each VM's NIC is a file handle, so the gateway lives where the VMs do). It builds on
// every platform; host_vz.go hands it vz.Native and the ioreg session on a Mac, tests the fakes.
func (h *Herdlet) macHost(api string, hv vz.Hypervisor, s vz.Session) (Backend, error) {
	label := h.MacHost
	if label == "" {
		n, err := os.Hostname()
		if err != nil {
			return Backend{}, err
		}
		label = strings.ToLower(strings.Split(n, ".")[0])
	}
	var hostNames []string
	if host, _, err := net.SplitHostPort(api); err == nil && host != "" {
		// herd-api is a host-side target: no actor reaches it through the gateway.
		hostNames = append(hostNames, host)
	}
	guard, err := egress.NewGuard(egress.GuardConfig{HostNames: hostNames})
	if err != nil {
		return Backend{}, err
	}
	ca, err := egress.NewCA("herd-egress " + label)
	if err != nil {
		return Backend{}, err
	}
	gw, err := egress.New(egress.Config{Guard: guard, CA: ca, OnEvent: func(e egress.Event) {
		if !e.Allowed {
			log.Printf("herd-egress: refused %s %s %s:%d host-side=%v: %s", e.Actor, e.Protocol, e.Name, e.Port, e.HostSide, e.Reason)
		}
	}})
	if err != nil {
		return Backend{}, err
	}
	nw := &egress.StackNetwork{Gateway: gw, Factory: gvstack.Factory{}}
	v, err := vz.New(vz.Config{
		StateDir: filepath.Join(h.StateDir, "vms"), SnapshotDir: filepath.Join(h.StateDir, "vm-snapshots"), Host: label,
		Probe:   vz.ProbeConfig{Kernel: h.Kernel, Initrd: h.Initrd, Cmdline: h.Cmdline},
		Session: s, Network: nw.Attach, ReleaseNetwork: nw.Release,
	}, hv)
	if err != nil {
		return Backend{}, fmt.Errorf("vz: %w", err)
	}
	return Backend{
		VMM: v, Guests: v, Version: "virtualization.framework",
		OnActor: func(vm string, a link.ActorRef) {
			nw.Assign(vm, egress.Actor{Space: a.Space, Name: a.Name, UID: a.UID})
		},
		Serve: func(ctx context.Context, policies grpc.ClientConnInterface) {
			sy := &egress.Syncer{Gateway: gw, Source: egress.ControlSource{Conn: policies},
				OnError: func(a egress.Actor, err error) { log.Printf("herd-egress: policy of %s: %v", a, err) }}
			_ = sy.Run(ctx)
		},
	}, nil
}
