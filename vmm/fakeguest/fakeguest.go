// Package fakeguest is a fake hypervisor whose VMs run the real herd-guest: a vmm/fake host where every running VM
// serves guest/agent (env's ProcessService and FileSystemService, herd's FileInfoService) on its own unix socket, in
// place of vsock. A VM's container is a local directory that outlives the VM, as a VM's volumes outlive a suspend;
// a delete stops the guest (cutting its calls) and a restore starts a fresh one on the same directory, so a client
// re-attaches to an exec by id the way it would after a real resume.
//
// It implements vmm.VMM and herdlet.GuestDialer, so a herdlet with it is a whole node for tests.
package fakeguest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/layer"
	"github.com/everdict/herd/guest/transport"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"google.golang.org/grpc"
)

// GuestPort is the port herd-guest serves its API on (vsock port 8080 in a real VM).
const GuestPort = 8080

// VMM is a fake host whose VMs serve a real herd-guest.
type VMM struct {
	*fake.Fake
	// Dir holds one directory per VM: root/ (the container) and guest.sock.
	Dir string
	// Shim is the command herd-guest starts detached execs with; the record directory is appended
	// (agent.Local.Shim).
	Shim []string
	// RootOf, if set, places vm's container at the directory it names instead of under Dir (e.g. the VM's share
	// as herdlet laid it out, so the guest's files are the volumes a suspend carries).
	RootOf func(vm string) string
	// GuestDelay, if set, is how long a VM's herd-guest takes to serve after its boot or restore was answered, as
	// under Cloud Hypervisor, which answers a boot before the guest's init runs: until then DialGuest refuses.
	GuestDelay time.Duration
	// ShimFor, if set, is the shim command of vm's guest in place of Shim (e.g. one that gives its processes the VM's
	// network).
	ShimFor func(vm string) []string
	// SyncFS, if set, is each guest's flush of one directory (agent.Guest.SyncFS); nil flushes nothing.
	SyncFS func(*os.File) error
	// PauseLag, if set, is how far a snapshot leaves a VM's wall clock behind, as Cloud Hypervisor stops the guest's clock
	// while it pauses the VM for one.
	PauseLag time.Duration

	mu      sync.Mutex
	guests  map[string]*guest
	sockMu  sync.Mutex // guards sockDir only: start calls sockPath with mu held
	sockDir string     // a short directory for sockets whose path under Dir would be too long

	// clocks is each VM's wall clock as an offset from the host's, which herd-guest's SetClock sets (agent.Guest.SetTime);
	// snapClocks is the wall clock each MEMORY snapshot's memory holds, which a restore of it runs on from.
	clocks     map[string]time.Duration
	snapClocks map[string]time.Time
}

// maxSockPath keeps a unix socket path under sun_path's limit (104 bytes on macOS, 108 on Linux).
const maxSockPath = 100

// sockPath is vm's guest socket: under Dir when the path fits, else in a short directory of its own under /tmp
// (not $TMPDIR, which macOS sets to a long /var/folders path).
func (v *VMM) sockPath(id string) (string, error) {
	if p := filepath.Join(v.Dir, id, "guest.sock"); len(p) <= maxSockPath {
		return p, nil
	}
	v.sockMu.Lock()
	defer v.sockMu.Unlock()
	if v.sockDir == "" {
		d, err := os.MkdirTemp("/tmp", "fg")
		if err != nil {
			return "", err
		}
		v.sockDir = d
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(v.sockDir, hex.EncodeToString(sum[:8])+".sock"), nil
}

type guest struct {
	g    *agent.Guest
	srv  *grpc.Server
	sock string
	done chan struct{}
	// ready is when the guest serves (GuestDelay after its start).
	ready time.Time
}

// New wraps f; dir must be short enough for unix socket paths.
func New(f *fake.Fake, dir string, shim []string) *VMM {
	return &VMM{Fake: f, Dir: dir, Shim: shim, guests: map[string]*guest{}, clocks: map[string]time.Duration{}, snapClocks: map[string]time.Time{}}
}

func (v *VMM) Boot(ctx context.Context, spec vmm.Spec) (vmm.VM, error) {
	vm, err := v.Fake.Boot(ctx, spec)
	if err != nil {
		return vm, err
	}
	v.setOffset(spec.ID, 0)
	// A booted guest's memory is new: its containers' roots start empty.
	if err := os.RemoveAll(filepath.Join(v.Dir, spec.ID, "containers")); err != nil {
		return vm, fmt.Errorf("fakeguest: %w", err)
	}
	return vm, v.start(spec.ID)
}

func (v *VMM) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	vm, err := v.Fake.Restore(ctx, s, spec)
	if err != nil {
		return vm, err
	}
	// A MEMORY restore runs on from the clock its snapshot's memory held; a VOLUMES one boots on the host's.
	v.mu.Lock()
	v.clocks[spec.ID] = 0
	if at, ok := v.snapClocks[s.ID]; ok && s.Fidelity == vmm.FidelityMemory {
		v.clocks[spec.ID] = time.Until(at)
	}
	v.mu.Unlock()
	return vm, v.start(spec.ID)
}

// Delete stops the VM's guest once the fake answers the VM gone.
func (v *VMM) Delete(ctx context.Context, id string) (vmm.Presence, error) {
	p, err := v.Fake.Delete(ctx, id)
	if p == vmm.PresenceGone {
		v.stop(id)
	}
	return p, err
}

// Root is the directory of vm's container.
func (v *VMM) Root(vm string) string {
	if v.RootOf != nil {
		return v.RootOf(vm)
	}
	return filepath.Join(v.Dir, vm, "root")
}

// targets are the guest's containers. A VM whose share holds a plan (herdlet's machines wrote it) gets one per
// container of the plan, each with its volumes where guest/initd binds them and a root directory of its own outside
// the share, as a real guest's containers have their rootfs in guest memory: a MEMORY restore under the VM's id
// keeps it (and the exec records in it), a boot starts it from the layer the snapshot's guest saved in the share
// (package layer), or empty. Any other VM has one container, "main", whose root is the share itself.
func (v *VMM) targets(id, root string, shim []string) ([]agent.Target, error) {
	l := initd.Layout{ShareDir: root}
	p, err := initd.ReadPlan(l)
	if err != nil {
		return []agent.Target{&agent.Local{ContainerName: "main", Root: root, Dir: "/workspace", Shim: shim}}, nil
	}
	vols, err := initd.Volumes(l, p)
	if err != nil {
		return nil, fmt.Errorf("fakeguest: %w", err)
	}
	var out []agent.Target
	for _, c := range p.Containers {
		r := filepath.Join(v.Dir, id, "containers", c.Name)
		layers := map[string]string{"upper": r}
		if _, err := os.Stat(r); errors.Is(err, fs.ErrNotExist) {
			// A root this VM's memory does not hold yet (a boot, or the restore of another VM's snapshot) starts as
			// the snapshot left the container: from the layer its guest saved, as guest/initd restores it, else empty.
			if err := os.MkdirAll(r, 0o755); err != nil {
				return nil, fmt.Errorf("fakeguest: %w", err)
			}
			if _, err := layer.Restore(root, c.Name, layers); err != nil {
				return nil, fmt.Errorf("fakeguest: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Join(r, "workspace"), 0o755); err != nil {
			return nil, fmt.Errorf("fakeguest: %w", err)
		}
		out = append(out, &agent.Local{ContainerName: c.Name, Root: r, Dir: "/workspace", Volumes: c.Mounts(vols), LayerDirs: layers, Shim: shim})
	}
	return out, nil
}

func (v *VMM) start(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.guests[id]; ok {
		return nil
	}
	root := v.Root(id)
	for _, d := range []string{filepath.Join(root, "workspace"), filepath.Join(v.Dir, id)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("fakeguest: %w", err)
		}
	}
	shim := v.Shim
	if v.ShimFor != nil {
		shim = v.ShimFor(id)
	}
	targets, err := v.targets(id, root, shim)
	if err != nil {
		return err
	}
	g, err := agent.New(targets...)
	if err != nil {
		return fmt.Errorf("fakeguest: %w", err)
	}
	// The container is a host directory: nothing sits between it and the share, so Sync has nothing to flush, and a
	// syncfs(2) of the host's disk on every snapshot would stall every other test's fsyncs.
	g.SyncFS = func(*os.File) error { return nil }
	// Nor are its caches its own: dropping them would drop the host's.
	g.DropCaches = func() error { return nil }
	if _, err := initd.ReadPlan(initd.Layout{ShareDir: root}); err == nil {
		// As herd-guest does in a VM: Sync saves each container's root into the share, where the bundle finds it.
		g.LayerShare = root
	}
	if v.SyncFS != nil {
		g.SyncFS = v.SyncFS
	}
	// The guest's clock is its own offset from the host's: herd-guest must not set the host's clock.
	g.SetTime = func(t time.Time) (time.Time, error) { return v.setTime(id, t) }
	sock, err := v.sockPath(id)
	if err != nil {
		g.Close()
		return fmt.Errorf("fakeguest: %w", err)
	}
	l, err := transport.ListenUnix(sock)
	if err != nil {
		g.Close()
		return fmt.Errorf("fakeguest: %w", err)
	}
	srv := grpc.NewServer()
	g.Register(srv)
	gu := &guest{g: g, srv: srv, sock: sock, done: make(chan struct{}), ready: time.Now().Add(v.GuestDelay)}
	go func() {
		defer close(gu.done)
		_ = srv.Serve(l)
	}()
	v.guests[id] = gu
	return nil
}

func (v *VMM) stop(id string) {
	v.mu.Lock()
	gu, ok := v.guests[id]
	delete(v.guests, id)
	v.mu.Unlock()
	if !ok {
		return
	}
	gu.srv.Stop()
	<-gu.done
	gu.g.Close()
}

// Close stops every guest.
func (v *VMM) Close() {
	v.mu.Lock()
	ids := make([]string, 0, len(v.guests))
	for id := range v.guests {
		ids = append(ids, id)
	}
	v.mu.Unlock()
	v.sockMu.Lock()
	sockDir := v.sockDir
	v.sockMu.Unlock()
	for _, id := range ids {
		v.stop(id)
	}
	if sockDir != "" {
		_ = os.RemoveAll(sockDir)
	}
}

// ErrNoGuest is what DialGuest answers for a VM that is not running or a port nothing listens on.
var ErrNoGuest = errors.New("fakeguest: nothing listens there")

// DialGuest connects to port of vm's guest; only GuestPort is served.
func (v *VMM) DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error) {
	v.mu.Lock()
	gu, ok := v.guests[vm]
	v.mu.Unlock()
	if !ok || port != GuestPort || time.Now().Before(gu.ready) {
		return nil, fmt.Errorf("%w: VM %s port %d", ErrNoGuest, vm, port)
	}
	return transport.DialUnix(gu.sock)(ctx)
}

// Snapshot records the wall clock a MEMORY snapshot's memory holds, and leaves the VM's clock behind by PauseLag.
func (v *VMM) Snapshot(ctx context.Context, id string, f vmm.Fidelity) (vmm.Snapshot, error) {
	at := v.Clock(id)
	s, err := v.Fake.Snapshot(ctx, id, f)
	if err != nil {
		return s, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s.Fidelity == vmm.FidelityMemory {
		v.snapClocks[s.ID] = at
	}
	v.clocks[id] -= v.PauseLag
	return s, nil
}

// AgeSnapshots makes every snapshot taken so far d older: its memory's clock is d further behind the host's, as a
// golden built d ago is.
func (v *VMM) AgeSnapshots(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, at := range v.snapClocks {
		v.snapClocks[id] = at.Add(-d)
	}
}

// Clock is vm's wall clock: what date(1) in its guest would read.
func (v *VMM) Clock(vm string) time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return time.Now().Add(v.clocks[vm])
}

func (v *VMM) setOffset(vm string, d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clocks[vm] = d
}

// setTime is vm's herd-guest stepping its wall clock to t (agent.Guest.SetTime): the host's own clock is not touched.
func (v *VMM) setTime(vm string, t time.Time) (time.Time, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	before := now.Add(v.clocks[vm])
	v.clocks[vm] = t.Sub(now)
	return before, nil
}

// Skew moves vm's wall clock by d, as a host clock stepped under a running VM leaves the guest's apart from it.
func (v *VMM) Skew(vm string, d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clocks[vm] += d
}
