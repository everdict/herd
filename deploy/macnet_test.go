package deploy

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/gvstack/gvvm"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vz/vzfake"
	"github.com/sirupsen/logrus"
)

// TestMacBackendNetworkIsHerdEgress (criterion vz-egress): the Mac backend as deploy.Herdlet assembles it, on the fake
// hypervisor. A booted actor VM's only NIC is the file handle of its own gvisor-tap-vsock stack; from inside it DNS
// is answered by herd-egress's synthetic DNS and an HTTP request is answered by herd-egress itself (deny-all until
// the actor's policy is read). After the VM is deleted its stack is closed and the actor unbound.
func TestMacBackendNetworkIsHerdEgress(t *testing.T) {
	logrus.SetOutput(io.Discard)
	dir := t.TempDir()
	g := struct{ Kernel, Initrd string }{"/guest/Image", "/guest/initrd.img"}
	hv := vzfake.New()
	h := &Herdlet{StateDir: filepath.Join(dir, "state"), Kernel: g.Kernel, Initrd: g.Initrd, MacHost: "mac-1"}
	be, err := h.macHost("herd-api.example:7443", hv, vzfake.UnlockedConsole())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := be.VMM.Capabilities(ctx)
	if err != nil || c.SuspendMaxFidelity != vmm.FidelityMemory || c.RestoreFamily != "microvm-vz-arm64-mac-1" {
		t.Fatalf("capabilities %+v %v", c, err)
	}
	for _, m := range hv.Machines() {
		if m.Config().Kernel != g.Kernel || m.Config().Network != nil {
			t.Errorf("the probe booted %q with a NIC %v, want this host's arm64 guest without one", m.Config().Kernel, m.Config().Network)
		}
	}

	be.OnActor("vm-1", link.ActorRef{Space: "space-1", Name: "mac-actor", UID: "u-1"})
	if _, err := be.VMM.Boot(ctx, vmm.Spec{ID: "vm-1", Kernel: g.Kernel, Initrd: g.Initrd, VCPUs: 1, MemoryMiB: 256}); err != nil {
		t.Fatal(err)
	}
	ms := hv.Machines()
	nic := ms[len(ms)-1].Config().Network
	if nic == nil {
		t.Fatal("the actor VM has no NIC")
	}
	// The guest configures itself from the plan the herdlet writes (Backend.Network, herd-guest's Plan.Network):
	// its address, its gateway and its resolver must be the ones the VM's stack serves.
	plan, pnic, err := be.Network.Lease(ctx, "vm-1", nil)
	if err != nil || pnic != nil {
		t.Fatalf("Mac lease: %+v %+v %v, want a plan network and no TAP", plan, pnic, err)
	}
	if err := (&initd.Plan{Containers: []initd.Container{{Name: "c", Image: initd.Image{Device: "/dev/vda", FSType: "erofs"}, Command: []string{"x"}}}, Network: &plan}).Validate(); err != nil {
		t.Fatalf("herd-guest would refuse the Mac plan: %v", err)
	}
	if got := ms[len(ms)-1].Config().NetworkMAC; plan.MAC == "" || got != plan.MAC {
		t.Errorf("the VM's NIC has MAC %q and the plan names %q: herd-guest finds its NIC by the plan's", got, plan.MAC)
	}
	addr, err := netip.ParsePrefix(plan.Address)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := gvvm.New(nic, addr.Addr(), netip.MustParseAddr(plan.Gateway))
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	if blk, _ := pem.Decode([]byte(plan.CA)); blk == nil {
		t.Errorf("the Mac plan carries no herd-egress CA: %q", plan.CA)
	} else if cert, err := x509.ParseCertificate(blk.Bytes); err != nil || !cert.IsCA || cert.Subject.CommonName != "herd-egress mac-1" {
		t.Errorf("the Mac plan's CA is not this host's gateway CA: %v %v", cert, err)
	}
	q := []byte{0xab, 0xcd, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'p', 'i', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 0, 1, 0, 1}
	r, err := vm.Exchange(netip.AddrPortFrom(netip.MustParseAddr(plan.DNS[0]), 53), q, 5*time.Second)
	if err != nil || netip.AddrFrom4([4]byte(r[len(r)-4:])) != egress.DefaultStackGateway {
		t.Fatalf("DNS from the VM: %x %v", r, err)
	}
	conn, err := vm.DialTCP(ctx, netip.MustParseAddrPort("203.0.113.9:80"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: api.example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	conn.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "herd-egress") {
		t.Errorf("HTTP from the VM: %d %q, want herd-egress's 403 (no policy read yet)", resp.StatusCode, body)
	}

	if p, err := be.VMM.Delete(ctx, "vm-1"); p != vmm.PresenceGone {
		t.Fatalf("Delete: %v %v", p, err)
	}
	dctx, dcancel := context.WithTimeout(ctx, time.Second)
	defer dcancel()
	if c, err := vm.DialTCP(dctx, netip.MustParseAddrPort("203.0.113.9:80")); err == nil {
		c.Close()
		t.Errorf("a deleted VM's link still carries connections")
	}
}

// TestMacHerdletSecrets (criterion mac-cred, join and launchd): a joined Mac's herdlet takes -secrets, and its
// in-process herd-egress (macHost, on the fake hypervisor) resolves ate-secret://dir/<name>/<tail> from that
// directory; a credential naming a store it does not have is refused as absent, and without -secrets every
// credential is refused for want of a store.
func TestMacHerdletSecrets(t *testing.T) {
	logrus.SetOutput(io.Discard)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(filepath.Join(secrets, "github"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "github", "token"), []byte("ghp-mac\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	actor := egress.Actor{Space: "space-1", Name: "mac-actor", UID: "u-1"}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"with -secrets", []string{"herd-api.example:7443", "-token-file", "/dev/null", "-secrets", secrets}, "ghp-mac"},
		{"without -secrets", []string{"herd-api.example:7443", "-token-file", "/dev/null"}, ""},
	} {
		var h Herdlet
		fs := flag.NewFlagSet("herdlet join", flag.ContinueOnError)
		h.JoinFlags(fs)
		if err := fs.Parse(c.args[1:]); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		h.StateDir, h.MacHost = filepath.Join(dir, c.name), "mac-1"
		var cfg egress.Config
		h.NewGateway = func(cf egress.Config) (*egress.Gateway, error) {
			cfg = cf
			return egress.New(cf)
		}
		if _, err := h.macHost(c.args[0], vzfake.New(), vzfake.UnlockedConsole()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == "" {
			if cfg.Credentials != nil {
				t.Errorf("%s: the gateway has credential stores %v", c.name, cfg.Credentials)
			}
			continue
		}
		if cfg.Credentials == nil {
			t.Fatalf("%s: the Mac gateway has no credential store", c.name)
		}
		if got, err := cfg.Credentials.Resolve(ctx, actor, "ate-secret://dir/github/token"); err != nil || got != c.want {
			t.Errorf("%s: ate-secret://dir/github/token = %q %v, want %q", c.name, got, err, c.want)
		}
		if _, err := cfg.Credentials.Resolve(ctx, actor, "ate-secret://vault/github/token"); !errors.Is(err, egress.ErrNoCredential) {
			t.Errorf("%s: a store the host does not have: %v, want ErrNoCredential", c.name, err)
		}
	}
}
