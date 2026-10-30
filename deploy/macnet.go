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
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
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
	guard, err := egress.NewGuard(egress.GuardConfig{HostNames: hostNames, LabAllow: h.EgressAllow})
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
		Session: s, Network: nw.Attach, ReleaseNetwork: nw.Release, NetworkMAC: egress.StackMAC,
	}, hv)
	if err != nil {
		return Backend{}, fmt.Errorf("vz: %w", err)
	}
	return Backend{
		VMM: v, Guests: v, Version: "virtualization.framework",
		Network: stackLink{ca: ca.CertPEM()},
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

// stackLink is a Mac's machine.Network (ADR A-21): every VM's link is the same fixed link into a stack of its own, so
// a lease differs between VMs only in its MAC (egress.StackMAC, which vz gives the NIC and by which the guest finds
// it), and the host has nothing to bring up or take down (vz releases the stack).
type stackLink struct {
	ca []byte
}

func (s stackLink) Lease(ctx context.Context, id string, want *initd.Network) (initd.Network, *vmm.NIC, error) {
	gw := egress.DefaultStackGateway.String()
	g := initd.Network{MAC: egress.StackMAC(id), Address: egress.DefaultStackVM.String() + "/24", Gateway: gw, DNS: []string{gw}, CA: string(s.ca)}
	if want != nil && (want.Address != g.Address || want.Gateway != g.Gateway) {
		return initd.Network{}, nil, fmt.Errorf("deploy: %s: a Mac's VMs are on %s via %s, not %s via %s", id, g.Address, gw, want.Address, want.Gateway)
	}
	return g, nil, nil
}

func (stackLink) Up(context.Context, string) error { return nil }

func (stackLink) Down(context.Context, string) (vmm.Presence, error) { return vmm.PresenceGone, nil }

func (stackLink) Release(context.Context, string) (vmm.Presence, error) { return vmm.PresenceGone, nil }
