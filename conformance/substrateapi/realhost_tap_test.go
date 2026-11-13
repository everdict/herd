package substrateapi_test

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/egress"
)

// realResolver resolves the test's names (listed.test, echo.test) as resolver does and everything else with the
// host's resolver, so the gateway of a real host still reaches the internet for whatever else a run lists.
type realResolver struct{}

func (realResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if a, ok := testName[host]; ok {
		return []netip.Addr{a}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// TestRealHostTAPEgress (criterion real-host-test; HERD_TAP_REAL=1, root, KVM, Cloud Hypervisor, virtiofsd, ip(8),
// nft(8), a guest kernel and initramfs from guest/build.sh, and a registry reachable for the image) runs herd
// all-in-one on the host's real backend: each actor is a Cloud Hypervisor VM on a TAP leased from HERD_REAL_POOL,
// with its egress table loaded before the link came up, an address, default route and resolver from its plan, and
// herd-egress's CA. From inside the guest, the egress walk reaches the listed host, is refused the unlisted echo
// host and the host-side targets (herd-api, herd-router, metadata), and flips on a policy update; the
// credential-header case has herd-egress fill the header. The gateway is the all-in-one's own, with only its
// resolver and dialer taking the test's names to loopback servers (as TestCredentialHeaderThroughAllInOneEgress).
// After the run, the egress cases' deleted actors hold no binding.
//
//	sudo -E env "PATH=$PATH" HERD_TAP_REAL=1 HERD_REAL_KERNEL=/usr/local/share/herd/kernel \
//	  HERD_REAL_INITRD=/usr/local/share/herd/initrd.img go test -count=1 -v -run TestRealHostTAPEgress ./conformance/substrateapi/
//
// Optional: HERD_REAL_IMAGE (default docker.io/library/busybox:1.36.1; it needs sleep and wget), HERD_REAL_CH
// (cloud-hypervisor), HERD_REAL_VIRTIOFSD (/usr/libexec/virtiofsd), HERD_REAL_POOL (10.89.0.0/24, away from a
// herd's default pool on the same host).
func TestRealHostTAPEgress(t *testing.T) {
	if os.Getenv("HERD_TAP_REAL") != "1" {
		t.Skip("set HERD_TAP_REAL=1 (root, KVM, cloud-hypervisor, virtiofsd, ip, nft, HERD_REAL_KERNEL, HERD_REAL_INITRD) to run real VMs on TAPs")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("TestRealHostTAPEgress needs root on Linux")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	kernel, initrd := os.Getenv("HERD_REAL_KERNEL"), os.Getenv("HERD_REAL_INITRD")
	if kernel == "" || initrd == "" {
		t.Fatal("HERD_REAL_KERNEL and HERD_REAL_INITRD must name the guest kernel and initramfs (guest/build.sh)")
	}
	data, err := os.MkdirTemp("", "herdtap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	n := newAllInOneVMNet(t)
	a := &deploy.AllInOne{Data: data, Listen: "127.0.0.1:0", RouterListen: "127.0.0.1:0", TunnelListen: "127.0.0.1:0",
		// herd-egress listens on every address: each VM's table redirects to the host's address on its TAP.
		EgressListen: ":0", DNSListen: ":0", Egress: true, Secrets: n.secrets,
		NewGateway: func(cfg egress.Config) (*egress.Gateway, error) {
			cfg.Resolver, cfg.Dialer = realResolver{}, dialer{}
			return n.NewGateway(cfg)
		},
		Herdlet: deploy.Herdlet{Worker: "realtap", Slots: 4, Kernel: kernel, Initrd: initrd, VCPUs: 1, MemoryMiB: 512,
			GuestPort: 8080, CHBinary: env("HERD_REAL_CH", "cloud-hypervisor"), Virtiofsd: env("HERD_REAL_VIRTIOFSD", "/usr/libexec/virtiofsd"),
			NetPool: env("HERD_REAL_POOL", "10.89.0.0/24"), Retry: 200 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan deploy.Addrs, 1)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, func(ad deploy.Addrs) { ready <- ad }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("all-in-one: %v", err)
		}
	})
	var addrs deploy.Addrs
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("all-in-one stopped: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("all-in-one not ready")
	}
	tok := aioToken(t, data, addrs)
	waitWorkers(t, addrs.API, tok, 1)
	c, err := substrateapi.DialControl(ep(addrs.API), tok)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = c.CreateActorTemplate(tctx, &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: env("HERD_REAL_IMAGE", "docker.io/library/busybox:1.36.1"),
			Command: []string{"sleep", "86400"}, VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}}},
		Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
	})
	tcancel()
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}

	cfg := suiteConfig("herd-real-tap", addrs.API, addrs.Router, tok)
	cfg.Wait = 2 * time.Minute
	e, echoURL, listedURL := echoes(t)
	optional(&cfg, e, echoURL, listedURL)
	cfg.EgressBound = 15 * time.Second
	cfg.LongStream, cfg.Template2 = false, ""
	// Before the suite: one actor's network, from inside its guest and from the host (netDiagnosis), so a failed
	// egress case below has its explanation in the log.
	netDiagnosis(t, c, cfg, addrs, data, listedURL)
	rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer rcancel()
	rep := substrateapi.Run(rctx, cfg)
	for _, r := range rep.Results {
		t.Logf("%s %s %s", r.Outcome, r.Case, r.Detail)
	}
	for _, name := range []string{"egress-walk", "credential-header"} {
		if r, _ := rep.Get(name); r.Outcome != substrateapi.Pass {
			t.Errorf("%s: %s %s; want PASS", name, r.Outcome, r.Detail)
			t.Log("see the net-diagnosis lines above: the guest's addresses, routes, resolver and DNS probe, and the host's TAP, table counters and firewall")
		}
	}

	// The egress cases delete their actors: no binding of theirs is left.
	var leases struct {
		Leases map[string]int `json:"leases"`
	}
	if b, err := os.ReadFile(filepath.Join(data, "herdlet", "net", "leases.json")); err == nil {
		_ = json.Unmarshal(b, &leases)
	}
	var bindings []map[string]string
	if b, err := os.ReadFile(filepath.Join(data, "egress-bindings.json")); err == nil {
		_ = json.Unmarshal(b, &bindings)
	}
	t.Logf("after the run: leases %v, bindings %v", leases.Leases, bindings)
	for _, b := range bindings {
		if strings.HasSuffix(b["name"], "-egress") || strings.HasSuffix(b["name"], "-cred") {
			t.Errorf("binding %v outlived its deleted actor", b)
		}
	}
}
