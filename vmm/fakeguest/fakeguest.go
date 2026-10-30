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
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/everdict/herd/guest/agent"
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

	mu     sync.Mutex
	guests map[string]*guest
}

type guest struct {
	g    *agent.Guest
	srv  *grpc.Server
	sock string
	done chan struct{}
}

// New wraps f; dir must be short enough for unix socket paths.
func New(f *fake.Fake, dir string, shim []string) *VMM {
	return &VMM{Fake: f, Dir: dir, Shim: shim, guests: map[string]*guest{}}
}

func (v *VMM) Boot(ctx context.Context, spec vmm.Spec) (vmm.VM, error) {
	vm, err := v.Fake.Boot(ctx, spec)
	if err != nil {
		return vm, err
	}
	return vm, v.start(spec.ID)
}

func (v *VMM) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	vm, err := v.Fake.Restore(ctx, s, spec)
	if err != nil {
		return vm, err
	}
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
	g, err := agent.New(&agent.Local{ContainerName: "main", Root: root, Dir: "/workspace", Shim: v.Shim})
	if err != nil {
		return fmt.Errorf("fakeguest: %w", err)
	}
	sock := filepath.Join(v.Dir, id, "guest.sock")
	l, err := transport.ListenUnix(sock)
	if err != nil {
		g.Close()
		return fmt.Errorf("fakeguest: %w", err)
	}
	srv := grpc.NewServer()
	g.Register(srv)
	gu := &guest{g: g, srv: srv, sock: sock, done: make(chan struct{})}
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
	for _, id := range ids {
		v.stop(id)
	}
}

// ErrNoGuest is what DialGuest answers for a VM that is not running or a port nothing listens on.
var ErrNoGuest = errors.New("fakeguest: nothing listens there")

// DialGuest connects to port of vm's guest; only GuestPort is served.
func (v *VMM) DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error) {
	v.mu.Lock()
	gu, ok := v.guests[vm]
	v.mu.Unlock()
	if !ok || port != GuestPort {
		return nil, fmt.Errorf("%w: VM %s port %d", ErrNoGuest, vm, port)
	}
	return transport.DialUnix(gu.sock)(ctx)
}
