package initd_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
)

// fakeNet records each call as an event, into the runtime's events when shared, so the order against container
// starts shows.
type fakeNet struct{ events *[]string }

func (f *fakeNet) log(s string) {
	if f.events == nil {
		f.events = new([]string)
	}
	*f.events = append(*f.events, s)
}

func (f *fakeNet) SetAddr(iface string, p netip.Prefix) error {
	f.log("addr " + iface + " " + p.String())
	return nil
}
func (f *fakeNet) Up(iface string) error { f.log("up " + iface); return nil }

func (f *fakeNet) AddDefaultRoute(iface string, gw netip.Addr) error {
	f.log("route " + iface + " via " + gw.String())
	return nil
}

// netEvents are f's events that are its own (not the runtime's).
func (f *fakeNet) netEvents() []string {
	var out []string
	if f.events != nil {
		for _, e := range *f.events {
			if !strings.HasPrefix(e, "run ") {
				out = append(out, e)
			}
		}
	}
	return out
}

// sysNet writes a /sys/class/net with these interfaces (name to MAC) and returns its path.
func sysNet(t *testing.T, ifaces map[string]string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "sys", "class", "net")
	for name, mac := range ifaces {
		if err := os.MkdirAll(filepath.Join(d, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name, "address"), []byte(mac+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// lo is ARPHRD_LOOPBACK; every other name here is a NIC: ARPHRD_ETHER with a device link.
		typ := "1"
		if name == "lo" {
			typ = "772"
		} else if err := os.MkdirAll(filepath.Join(d, name, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name, "type"), []byte(typ+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// testCA is a self-signed CA certificate in PEM.
func testCA(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "herd-egress test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// planWith is planJSON with n as its network.
func planWith(t *testing.T, n *initd.Network) string {
	t.Helper()
	var p initd.Plan
	if err := json.Unmarshal([]byte(planJSON), &p); err != nil {
		t.Fatal(err)
	}
	p.Network = n
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNetworkValidation(t *testing.T) {
	ca := testCA(t)
	good := initd.Network{MAC: "52:54:00:12:34:56", Address: "10.88.0.2/30", Gateway: "10.88.0.1",
		DNS: []string{"10.88.0.1", "fd00::1"}, CA: ca}
	if err := good.Validate(); err != nil {
		t.Fatalf("good network refused: %v", err)
	}
	wide := initd.Network{Address: "192.168.64.2/24", Gateway: "192.168.64.1", CA: ca + ca}
	if err := wide.Validate(); err != nil {
		t.Fatalf("a /24 with no MAC and a two-certificate CA refused: %v", err)
	}
	bad := map[string]func(n *initd.Network){
		"address not a prefix":     func(n *initd.Network) { n.Address = "10.88.0.2" },
		"address garbage":          func(n *initd.Network) { n.Address = "nope/30" },
		"address IPv6":             func(n *initd.Network) { n.Address = "fd00::2/126" },
		"address /31":              func(n *initd.Network) { n.Address, n.Gateway = "10.88.0.2/31", "10.88.0.3" },
		"address /32":              func(n *initd.Network) { n.Address = "10.88.0.2/32" },
		"address is network":       func(n *initd.Network) { n.Address = "10.88.0.0/30" },
		"address is broadcast":     func(n *initd.Network) { n.Address = "10.88.0.3/30" },
		"gateway empty":            func(n *initd.Network) { n.Gateway = "" },
		"gateway outside prefix":   func(n *initd.Network) { n.Gateway = "10.88.0.5" },
		"gateway is the address":   func(n *initd.Network) { n.Gateway = "10.88.0.2" },
		"gateway is the broadcast": func(n *initd.Network) { n.Gateway = "10.88.0.3" },
		"gateway IPv6":             func(n *initd.Network) { n.Gateway = "fd00::1" },
		"dns not an IP":            func(n *initd.Network) { n.DNS = []string{"10.88.0.1", "dns.example"} },
		"dns with a zone":          func(n *initd.Network) { n.DNS = []string{"fe80::1%eth0"} },
		"mac garbage":              func(n *initd.Network) { n.MAC = "52:54:00:12:34" },
		"ca not PEM":               func(n *initd.Network) { n.CA = "not a certificate" },
		"ca a key":                 func(n *initd.Network) { n.CA = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n" },
		"ca bad DER":               func(n *initd.Network) { n.CA = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n" },
		"ca trailing garbage":      func(n *initd.Network) { n.CA = ca + "garbage" },
	}
	for name, mut := range bad {
		n := good
		mut(&n)
		if err := n.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := initd.ParsePlan([]byte(planWith(t, &n))); err == nil {
			t.Errorf("%s: plan accepted", name)
		}
	}
	unknown := strings.Replace(planWith(t, &good), `"gateway"`, `"mtu":1500,"gateway"`, 1)
	if _, err := initd.ParsePlan([]byte(unknown)); err == nil {
		t.Error("network with an unknown field accepted")
	}
}

func TestConfigureNetworkFindsTheInterface(t *testing.T) {
	ifaces := map[string]string{"lo": "00:00:00:00:00:00", "eth0": "02:00:00:00:00:01", "eth1": "52:54:00:ab:cd:ef"}
	n := &initd.Network{MAC: "52:54:00:AB:CD:EF", Address: "10.88.0.2/30", Gateway: "10.88.0.1"}
	nl := &fakeNet{}
	if err := initd.ConfigureNetwork(nl, sysNet(t, ifaces), n); err != nil {
		t.Fatal(err)
	}
	want := "up lo,addr eth1 10.88.0.2/30,up eth1,route eth1 via 10.88.0.1"
	if got := strings.Join(nl.netEvents(), ","); got != want {
		t.Errorf("by MAC: calls = %s, want %s", got, want)
	}

	nl = &fakeNet{}
	only := &initd.Network{Address: "10.88.0.6/30", Gateway: "10.88.0.5"}
	if err := initd.ConfigureNetwork(nl, sysNet(t, map[string]string{"lo": "00:00:00:00:00:00", "enp0s1": "02:00:00:00:00:01"}), only); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(nl.netEvents(), ","), "up lo,addr enp0s1 10.88.0.6/30,up enp0s1,route enp0s1 via 10.88.0.5"; got != want {
		t.Errorf("only interface: calls = %s, want %s", got, want)
	}

	for name, c := range map[string]struct {
		ifaces map[string]string
		n      *initd.Network
	}{
		"two interfaces, no MAC": {ifaces, only},
		"only lo":                {map[string]string{"lo": "00:00:00:00:00:00"}, only},
		"no interface has MAC":   {ifaces, &initd.Network{MAC: "52:54:00:00:00:99", Address: "10.88.0.2/30", Gateway: "10.88.0.1"}},
		"invalid network":        {ifaces, &initd.Network{MAC: n.MAC, Address: "10.88.0.2/30", Gateway: "10.88.0.9"}},
	} {
		nl := &fakeNet{}
		if err := initd.ConfigureNetwork(nl, sysNet(t, c.ifaces), c.n); err == nil {
			t.Errorf("%s: no error", name)
		}
		if ev := nl.netEvents(); len(ev) != 0 {
			t.Errorf("%s: configured anyway: %v", name, ev)
		}
	}
}

// Counterexample: with no network step in Boot, the containers start on a VM with no address and no route.
func TestBootConfiguresNetworkBeforeContainers(t *testing.T) {
	l := layout(t)
	_, rt, _, err := boot(t, l, planWith(t, &initd.Network{MAC: "52:54:00:12:34:56", Address: "10.88.0.2/30",
		Gateway: "10.88.0.1", DNS: []string{"10.88.0.1"}}))
	if err != nil {
		t.Fatal(err)
	}
	want := "up lo,addr eth0 10.88.0.2/30,up eth0,route eth0 via 10.88.0.1,run main,run sidecar"
	if got := strings.Join(rt.events, ","); got != want {
		t.Fatalf("no network configured before the containers: events = %s, want %s", got, want)
	}
}

func TestBootWithoutNetworkLeavesItAlone(t *testing.T) {
	l := layout(t)
	_, rt, _, err := boot(t, l, planJSON)
	if err != nil {
		t.Fatal(err)
	}
	if ev := rt.net.netEvents(); len(ev) != 0 {
		t.Errorf("a plan without network configured one: %v", ev)
	}
	if _, err := os.Stat(filepath.Join(l.RunDir, "net")); err == nil {
		t.Error("a plan without network wrote guest network files")
	}
	for id, s := range rt.specs {
		for _, m := range s.Mounts {
			if strings.HasPrefix(m.Destination, "/etc/") {
				t.Errorf("container %s got %s without a network", id, m.Destination)
			}
		}
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// fileMount returns s's mount at dst, checking it is a read-only bind.
func fileMount(t *testing.T, id string, s *oci.Spec, dst string) (oci.Mount, bool) {
	t.Helper()
	for _, m := range s.Mounts {
		if m.Destination != dst {
			continue
		}
		if m.Type != "bind" || !strings.Contains(","+strings.Join(m.Options, ",")+",", ",ro,") {
			t.Errorf("container %s: %s is not a read-only bind: %+v", id, dst, m)
		}
		return m, true
	}
	return oci.Mount{}, false
}

// Counterexample: without the guest files in the spec, a container resolves with whatever /etc/resolv.conf its
// image has and trusts none of herd-egress's certificates.
func TestContainersGetResolverAndCA(t *testing.T) {
	l := layout(t)
	ca := testCA(t)
	// main: a Debian bundle, Alpine's relative link to it, and Fedora's absolute link inside the image.
	main := filepath.Join(l.RunDir, "containers", "main", "rootfs")
	writeFile(t, filepath.Join(main, "etc/ssl/certs/ca-certificates.crt"), "IMAGE-DEBIAN-BUNDLE\n")
	symlink(t, "certs/ca-certificates.crt", filepath.Join(main, "etc/ssl/cert.pem"))
	writeFile(t, filepath.Join(main, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"), "IMAGE-FEDORA-BUNDLE")
	symlink(t, "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", filepath.Join(main, "etc/pki/tls/certs/ca-bundle.crt"))
	// sidecar: every bundle path a link out of its rootfs, to a file of the guest.
	outside := filepath.Join(t.TempDir(), "secret")
	writeFile(t, outside, "GUEST-SECRET")
	side := filepath.Join(l.RunDir, "containers", "sidecar", "rootfs")
	symlink(t, outside, filepath.Join(side, "etc/ssl/certs/ca-certificates.crt"))
	symlink(t, strings.Repeat("../", 20)+strings.TrimPrefix(outside, "/"), filepath.Join(side, "etc/pki/tls/certs/ca-bundle.crt"))
	symlink(t, "/.."+strings.Repeat("/..", 20)+outside, filepath.Join(side, "etc/ssl/cert.pem"))

	_, rt, _, err := boot(t, l, planWith(t, &initd.Network{Address: "10.88.0.2/30", Gateway: "10.88.0.1",
		DNS: []string{"10.88.0.1", "1.1.1.1"}, CA: ca}))
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, id := range []string{"main", "sidecar"} {
		s := rt.specs[id]
		m, ok := fileMount(t, id, s, "/etc/resolv.conf")
		if !ok {
			t.Fatalf("container %s: no resolv.conf mount", id)
		}
		if got := read(m.Source); got != "nameserver 10.88.0.1\nnameserver 1.1.1.1\n" {
			t.Errorf("container %s: resolv.conf = %q", id, got)
		}
		if m, ok := fileMount(t, id, s, initd.EgressCAPath); !ok || read(m.Source) != ca {
			t.Errorf("container %s: herd-egress's CA not at %s", id, initd.EgressCAPath)
		}
		for _, e := range s.Process.Env {
			if strings.HasPrefix(e, "SSL_CERT") {
				t.Errorf("container %s: env touched: %s", id, e)
			}
		}
		for _, m := range s.Mounts {
			if m.Type == "bind" && strings.HasPrefix(m.Source, l.RunDir+"/") && strings.Contains(read(m.Source), "GUEST-SECRET") {
				t.Errorf("container %s: %s holds a guest file read through a link out of the rootfs", id, m.Destination)
			}
		}
	}
	s := rt.specs["main"]
	for dst, image := range map[string]string{
		"/etc/ssl/certs/ca-certificates.crt":                "IMAGE-DEBIAN-BUNDLE\n",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem": "IMAGE-FEDORA-BUNDLE",
	} {
		m, ok := fileMount(t, "main", s, dst)
		if !ok {
			t.Errorf("main: no merged bundle at %s", dst)
			continue
		}
		if got := read(m.Source); got != image+"\n"+ca {
			t.Errorf("main: bundle at %s = %q, want the image's then the CA", dst, got)
		}
	}
	if n := countTrust(s, l.RunDir); n != 3 {
		t.Errorf("main: %d trust mounts, want 3 (two merged bundles, the CA alone; the Alpine link shares one)", n)
	}
	s = rt.specs["sidecar"]
	if m, ok := fileMount(t, "sidecar", s, "/etc/ssl/certs/ca-certificates.crt"); !ok || read(m.Source) != ca {
		t.Errorf("sidecar: with no bundle of its own, the CA alone is not its bundle: %+v", m)
	}
	if n := countTrust(s, l.RunDir); n != 2 {
		t.Errorf("sidecar: %d trust mounts, want 2", n)
	}
}

func countTrust(s *oci.Spec, run string) int {
	n := 0
	for _, m := range s.Mounts {
		if m.Type == "bind" && strings.HasPrefix(m.Source, run+"/") && m.Destination != "/etc/resolv.conf" {
			n++
		}
	}
	return n
}

// TestConfigureNetworkSkipsTunnels: the arm64 guest kernel makes sit0 on its own (a Mac run: "no mac given and 2
// interfaces besides lo: [eth0 sit0]", then init died). With no MAC in the plan the only Ethernet NIC is chosen;
// sit0 (ARPHRD_SIT, no device) and a bridge-like virtual Ethernet device without a device link are not NICs.
func TestConfigureNetworkSkipsTunnels(t *testing.T) {
	d := sysNet(t, map[string]string{"lo": "00:00:00:00:00:00", "eth0": "5e:12:34:56:78:9a"})
	for name, typ := range map[string]string{"sit0": "776", "tunl0": "768", "dummy0": "1"} {
		if err := os.MkdirAll(filepath.Join(d, name), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(d, name, "address"), []byte("00:00:00:00\n"), 0o644)
		_ = os.WriteFile(filepath.Join(d, name, "type"), []byte(typ+"\n"), 0o644)
	}
	nl := &fakeNet{}
	if err := initd.ConfigureNetwork(nl, d, &initd.Network{Address: "192.168.127.2/24", Gateway: "192.168.127.1"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(nl.netEvents(), ","), "up lo,addr eth0 192.168.127.2/24,up eth0,route eth0 via 192.168.127.1"; got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}
