package machine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// guestBootVMM is a hypervisor whose guest runs herd-guest's network step from the plan in the VM's share, on a
// sysfs with two Ethernet NICs, and on failure does what herd-guest does: records why in the share and ends the
// VM, so the start fails (as vz's does when the VM stops before its guest answers).
type guestBootVMM struct {
	*fake.Fake
	sys string
}

type noNet struct{}

func (noNet) SetAddr(string, netip.Prefix) error       { return nil }
func (noNet) Up(string) error                          { return nil }
func (noNet) AddDefaultRoute(string, netip.Addr) error { return nil }

func (g guestBootVMM) Boot(ctx context.Context, s vmm.Spec) (vmm.VM, error) {
	l := initd.Layout{ShareDir: s.Shares[len(s.Shares)-1].HostPath}
	p, err := initd.ReadPlan(l)
	if err == nil && p.Network != nil {
		err = initd.ConfigureNetwork(noNet{}, g.sys, p.Network)
	}
	if err != nil {
		if werr := initd.WriteBootError(l, err); werr != nil {
			return vmm.VM{}, werr
		}
		return vmm.VM{}, fmt.Errorf("vz: %s: the VM stopped before its guest answered on vsock port 8080", s.ID)
	}
	return g.Fake.Boot(ctx, s)
}

// staticNet leases every VM a plan network without a MAC, as a Mac's did before its NIC had one.
type staticNet struct{}

func (staticNet) Lease(context.Context, string, *initd.Network) (initd.Network, *vmm.NIC, error) {
	return initd.Network{Address: "192.168.127.2/24", Gateway: "192.168.127.1", DNS: []string{"192.168.127.1"}}, nil, nil
}
func (staticNet) Up(context.Context, string) error { return nil }
func (staticNet) Down(context.Context, string) (vmm.Presence, error) {
	return vmm.PresenceGone, nil
}
func (staticNet) Release(context.Context, string) (vmm.Presence, error) {
	return vmm.PresenceGone, nil
}

// TestGuestBootFailureIsFastAndTyped: a network step herd-guest cannot carry out (two NICs, no MAC to tell them
// apart) fails the boot at once with ErrGuestBoot and the guest's own reason, instead of a guest that never
// answers; nothing of the VM is left.
func TestGuestBootFailureIsFastAndTyped(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	sys := filepath.Join(t.TempDir(), "net")
	for _, nic := range []string{"eth0", "eth1"} {
		if err := os.MkdirAll(filepath.Join(sys, nic, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(sys, nic, "type"), []byte("1\n"), 0o644)
		_ = os.WriteFile(filepath.Join(sys, nic, "address"), []byte("02:00:00:00:00:0"+nic[3:]+"\n"), 0o644)
	}
	h.m.cfg.VMM = guestBootVMM{Fake: h.vm.Fake, sys: sys}
	h.m.cfg.Network = staticNet{}
	start := time.Now()
	err := h.m.Boot(context.Background(), "actor-1", template())
	if !errors.Is(err, ErrGuestBoot) || !strings.Contains(err.Error(), "no mac given and 2 Ethernet interfaces") {
		t.Fatalf("Boot: %v, want ErrGuestBoot with the guest's reason", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the failed boot took %v", d)
	}
	if _, serr := os.Stat(h.m.ShareDir("actor-1")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the failed VM's share is left: %v", serr)
	}
}
