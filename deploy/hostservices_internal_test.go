package deploy

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
)

// TestHostServicesFileFlag (declared-host-service): herd-egress and herd take -host-services-file; herd-egress refuses
// to start on a file it cannot read or parse, and names its -api and -router endpoints to the gateway so a host
// service naming one is dropped.
func TestHostServicesFileFlag(t *testing.T) {
	var e Egress
	var a AllInOne
	for name, reg := range map[string]func(*flag.FlagSet){"herd-egress": e.Flags, "herd": a.Flags} {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		reg(fs)
		args := []string{"-host-services-file", "/etc/herd/host-services"}
		if name == "herd-egress" {
			args = append(args, "-router", "10.0.0.8:8443,10.0.0.9:8443", "-router", "", "-api", "10.0.0.7:7443")
		}
		if err := fs.Parse(args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if e.HostServicesFile != "/etc/herd/host-services" || a.HostServicesFile != "/etc/herd/host-services" {
		t.Errorf("-host-services-file gave %q and %q", e.HostServicesFile, a.HostServicesFile)
	}
	eps, err := e.endpoints()
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.7:7443"), netip.MustParseAddrPort("10.0.0.8:8443"), netip.MustParseAddrPort("10.0.0.9:8443")}
	if len(eps) != len(want) {
		t.Fatalf("endpoints %v, want %v", eps, want)
	}
	for i := range want {
		if eps[i] != want[i] {
			t.Errorf("endpoints %v, want %v", eps, want)
		}
	}

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("relay.herd.internal 10.0.0.5:8787\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing"), bad} {
		eg := Egress{Bindings: filepath.Join(dir, "bindings.json"), HostServicesFile: path, CAOut: filepath.Join(dir, "ca.pem")}
		if err := eg.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "-host-services-file") {
			t.Errorf("herd-egress -host-services-file %s: Run = %v, want a refusal at start", path, err)
		}
	}
}

// TestHostServicesFileReload (declared-host-service): the file is re-read on each reload; a change adds and removes
// services, a line naming a refused target is dropped with a log, and a malformed or unreadable file keeps the last
// good set, logged once.
func TestHostServicesFileReload(t *testing.T) {
	guard, err := egress.NewGuard(egress.GuardConfig{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := egress.New(egress.Config{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	path := filepath.Join(t.TempDir(), "host-services")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names := func() string {
		var out []string
		for _, s := range g.HostServices() {
			out = append(out, s.String())
		}
		return strings.Join(out, ",")
	}
	f := &hostServicesFile{path: path, g: g}

	write("# none yet\n")
	if err := f.load(); err != nil {
		t.Fatal(err)
	}
	write("a.herd.internal:80 10.0.0.1:80\nmeta.herd.internal:80 169.254.169.254:80\n")
	f.reload()
	if got := names(); got != "a.herd.internal:80 -> 10.0.0.1:80" {
		t.Errorf("after adding a: %q", got)
	}
	if !strings.Contains(buf.String(), "metadata endpoint") {
		t.Errorf("the dropped metadata line was not logged: %q", buf.String())
	}
	write("b.herd.internal:80 10.0.0.2:80\n")
	f.reload()
	if got := names(); got != "b.herd.internal:80 -> 10.0.0.2:80" {
		t.Errorf("after replacing a with b: %q", got)
	}
	buf.Reset()
	write("b.herd.internal 10.0.0.2:80\n")
	f.reload()
	f.reload()
	if got := names(); got != "b.herd.internal:80 -> 10.0.0.2:80" {
		t.Errorf("after a malformed file: %q, want the last good set", got)
	}
	if n := strings.Count(buf.String(), "keeping the last good set"); n != 1 {
		t.Errorf("a malformed file was logged %d times, want once: %q", n, buf.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.reload()
	if got := names(); got != "b.herd.internal:80 -> 10.0.0.2:80" {
		t.Errorf("after the file went away: %q, want the last good set", got)
	}
}

// TestHostServicesFileRun (declared-host-service): a running herd-egress re-reads -host-services-file on its
// -interval, in the loop that re-reads the bindings, so a service added or removed reaches the gateway.
func TestHostServicesFileRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host-services")
	if err := os.WriteFile(path, []byte("a.herd.internal:80 10.0.0.1:80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gw := make(chan *egress.Gateway, 1)
	eg := &Egress{API: "127.0.0.1:1", Token: "t", Identity: testEgressIdentity(t), Worker: "node-1", Bindings: filepath.Join(dir, "bindings.json"), CAOut: filepath.Join(dir, "ca.pem"),
		DNSAnswer: "198.18.0.1", Interval: 20 * time.Millisecond, HostServicesFile: path,
		NewGateway: func(c egress.Config) (*egress.Gateway, error) {
			g, err := egress.New(c)
			gw <- g
			return g, err
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eg.Run(ctx, ln, pc) }()
	defer func() { cancel(); <-done }()
	g := <-gw
	waitFor := func(want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			var s []string
			for _, h := range g.HostServices() {
				s = append(s, h.String())
			}
			if got = strings.Join(s, ","); got == want {
				return
			}
		}
		t.Fatalf("host services %q, want %q", got, want)
	}
	waitFor("a.herd.internal:80 -> 10.0.0.1:80")
	if err := os.WriteFile(path, []byte("b.herd.internal:80 10.0.0.2:80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("b.herd.internal:80 -> 10.0.0.2:80")
}
