//go:build darwin && arm64 && cgo

package vz_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vz"
)

// TestReal boots a real guest through Virtualization.framework. It runs only with HERD_VZ_REAL=1 and needs
// HERD_VZ_KERNEL (the arm64 Image) and HERD_VZ_INITRD (an initramfs whose /init is internal/vzguest); the test
// binary must be signed with com.apple.security.virtualization. realtest.sh builds and signs all of it.
func TestReal(t *testing.T) {
	if os.Getenv("HERD_VZ_REAL") != "1" {
		t.Skip("set HERD_VZ_REAL=1 with HERD_VZ_KERNEL and HERD_VZ_INITRD to boot a real guest (see realtest.sh)")
	}
	kernel, initrd := os.Getenv("HERD_VZ_KERNEL"), os.Getenv("HERD_VZ_INITRD")
	if kernel == "" || initrd == "" {
		t.Fatal("HERD_VZ_REAL=1 needs HERD_VZ_KERNEL and HERD_VZ_INITRD")
	}
	host := os.Getenv("HERD_VZ_HOST")
	if host == "" {
		host = "real-mac"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	state := t.TempDir()
	hv := &recorder{Hypervisor: vz.Native()}
	v, err := vz.New(vz.Config{StateDir: state, Host: host, Probe: vz.ProbeConfig{Kernel: kernel, Initrd: initrd}, Session: vz.IORegSession{}}, hv)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := v.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("capabilities %+v; probe %+v", caps, v.Probe(ctx))

	share := t.TempDir()
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.WriteFile(filepath.Join(share, "in.txt"), []byte(token), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := vmm.Spec{ID: "real1", Kernel: kernel, Initrd: initrd, Cmdline: "console=hvc0", VCPUs: 1, MemoryMiB: 512,
		Shares: []vmm.Share{{Tag: "herd", HostPath: share}}}
	start := time.Now()
	if _, err := v.Boot(ctx, spec); err != nil {
		console, _ := os.ReadFile(filepath.Join(state, "vms", "real1", "console.log"))
		t.Fatalf("Boot: %v\nconsole:\n%s", err, console)
	}
	t.Logf("boot to guest answering on vsock: %v", time.Since(start))
	t.Cleanup(func() { _, _ = v.Delete(context.Background(), "real1") })
	booted := hv.last()

	// virtio-fs, host to guest and back.
	if got := request(t, ctx, v, "read in.txt\n"); got != token {
		t.Errorf("guest read in.txt = %q, want %q", got, token)
	}
	if got := request(t, ctx, v, "write out.txt from-guest-"+token+"\n"); got != "ok" {
		t.Errorf("guest write = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(share, "out.txt")); err != nil || string(b) != "from-guest-"+token {
		t.Errorf("host read out.txt = %q (%v)", b, err)
	}
	// A byte stream over native vsock.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	c, err := v.DialGuest(ctx, "real1", vz.DefaultGuestPort)
	if err != nil {
		t.Fatal(err)
	}
	// The guest echoes until the host closes; read exactly the payload back, then close.
	go func() { _, _ = c.Write(append([]byte("echo\n"), payload...)) }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	echoed, err := io.ReadAll(io.LimitReader(c, int64(len(payload))))
	c.Close()
	if err != nil || string(echoed) != string(payload) {
		t.Errorf("vsock echo: %d of %d bytes back (%v)", len(echoed), len(payload), err)
	}

	// Suspend at the declared fidelity: snapshot, then verified delete.
	snap, err := v.Snapshot(ctx, "real1", caps.SuspendMaxFidelity)
	if err != nil {
		t.Fatalf("Snapshot(%v): %v", caps.SuspendMaxFidelity, err)
	}
	t.Logf("snapshot %+v", snap)
	if caps.SuspendMaxFidelity < vmm.FidelityMemory {
		if _, err := v.Snapshot(ctx, "real1", vmm.FidelityMemory); !errors.Is(err, vmm.ErrFidelityUnsupported) {
			t.Errorf("Snapshot(MEMORY) on a VOLUMES Mac = %v, want ErrFidelityUnsupported", err)
		}
	}
	if p, err := v.Delete(ctx, "real1"); p != vmm.PresenceGone || err != nil {
		t.Fatalf("Delete = %v (%v), want gone", p, err)
	}
	if _, err := os.Stat(filepath.Join(state, "vms", "real1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gone, but the VM's files remain: %v", err)
	}
	assertReallyStopped(t, "Delete after Boot", booted)

	// Resume: the volume is still there.
	if _, err := v.Restore(ctx, snap, spec); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := hv.last()
	if got := request(t, ctx, v, "read out.txt\n"); got != "from-guest-"+token {
		t.Errorf("after restore, guest read out.txt = %q", got)
		diagnoseShare(t, ctx, v, share, token)
	}
	if p, err := v.Delete(ctx, "real1"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete after restore = %v (%v), want gone", p, err)
	}
	assertReallyStopped(t, "Delete after Restore", restored)
}

// recorder keeps every machine the backend builds, so the test can ask Virtualization.framework about a VM
// directly instead of trusting the backend's own answers.
type recorder struct {
	vz.Hypervisor

	mu       sync.Mutex
	machines []vz.Machine
}

func (r *recorder) NewMachine(cfg vz.MachineConfig) (vz.Machine, error) {
	m, err := r.Hypervisor.NewMachine(cfg)
	if err == nil {
		r.mu.Lock()
		r.machines = append(r.machines, m)
		r.mu.Unlock()
	}
	return m, err
}

// last is the machine built most recently.
func (r *recorder) last() vz.Machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.machines) == 0 {
		return nil
	}
	return r.machines[len(r.machines)-1]
}

// assertReallyStopped checks, independently of Delete's answer, that a VM Delete called gone is not running:
// Virtualization.framework reports it stopped (or in error) and its guest no longer accepts vsock connections.
func assertReallyStopped(t *testing.T, what string, m vz.Machine) {
	t.Helper()
	if m == nil {
		t.Errorf("%s: no machine was recorded", what)
		return
	}
	st, err := m.State()
	if err != nil || (st != vz.MachineStopped && st != vz.MachineError) {
		t.Errorf("%s: Delete answered gone, but Virtualization.framework reports the VM %v (%v): a running VM was leaked", what, st, err)
	}
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := m.ConnectVsock(vz.DefaultGuestPort)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			r.c.Close()
			t.Errorf("%s: Delete answered gone, but the guest still accepts vsock connections on port %d: a running VM was leaked", what, vz.DefaultGuestPort)
		}
	case <-time.After(5 * time.Second):
		// A connect that never completes did not reach a guest.
	}
}

// diagnoseShare logs what tells apart the explanations for a restored guest that cannot read a file it wrote to
// its share before the snapshot (PLT-199): the host directory lost the file; the restored virtio-fs device does not
// serve that directory; or the device serves it and only the guest's cached view of names it looked up before the
// snapshot (dentries and FUSE node ids of the saved session) no longer resolves.
func diagnoseShare(t *testing.T, ctx context.Context, v *vz.VMM, share, token string) {
	t.Helper()
	var host []string
	es, err := os.ReadDir(share)
	for _, e := range es {
		host = append(host, e.Name())
	}
	t.Logf("PLT-199: host share %s holds %q (%v)", share, host, err)
	b, err := os.ReadFile(filepath.Join(share, "out.txt"))
	hostHas := err == nil && string(b) == "from-guest-"+token
	// A name the guest never looked up before the snapshot: answering it needs a fresh lookup on the share.
	if err := os.WriteFile(filepath.Join(share, "fresh.txt"), []byte("fresh-"+token), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := request(t, ctx, v, "read fresh.txt\n")
	t.Logf("PLT-199: guest read fresh.txt (written by the host after restore) = %q", fresh)
	t.Logf("PLT-199: guest read in.txt (looked up before the snapshot) = %q", request(t, ctx, v, "read in.txt\n"))
	t.Logf("PLT-199: guest lists the share: %q", request(t, ctx, v, "list\n"))
	t.Logf("PLT-199: guest drop caches = %q", request(t, ctx, v, "dropcaches\n"))
	afterDrop := request(t, ctx, v, "read out.txt\n")
	t.Logf("PLT-199: guest read out.txt after dropping caches = %q", afterDrop)
	switch {
	case !hostHas:
		t.Logf("PLT-199 verdict: the host directory itself lost out.txt (%q, %v)", b, err)
	case fresh != "fresh-"+token:
		t.Logf("PLT-199 verdict: the host directory holds out.txt, but the restored virtio-fs device does not serve it (a fresh name does not resolve either)")
	case afterDrop == "from-guest-"+token:
		t.Logf("PLT-199 verdict: the device serves the snapshot-time directory; the guest's cached lookups from the saved session were stale, and dropping them fixed the read")
	default:
		t.Logf("PLT-199 verdict: the device serves the directory (a fresh name resolves), but names the guest held before the snapshot stay unresolvable even after dropping caches")
	}
}

// request sends one vzguest request and returns the whole answer.
func request(t *testing.T, ctx context.Context, v *vz.VMM, line string) string {
	t.Helper()
	c, err := v.DialGuest(ctx, "real1", vz.DefaultGuestPort)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatalf("request %q: %v", line, err)
	}
	b, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("request %q: %v", line, err)
	}
	return string(b)
}
