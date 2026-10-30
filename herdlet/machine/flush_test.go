package machine

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bufferedGuest is a guest whose writes to its volumes stay in its memory until herd-guest's Sync writes them back
// to the share, as a guest's page cache over virtio-fs may hold them. It serves herd.ext.v1.SyncService on a unix
// socket, which DialGuest reaches.
type bufferedGuest struct {
	share string
	sock  string
	// answer, if set, is what Sync answers instead of writing back; hang makes Sync never answer.
	answer error
	hang   bool

	mu      sync.Mutex
	pending map[string]string
	syncs   int
}

func newBufferedGuest(t *testing.T, share string) *bufferedGuest {
	t.Helper()
	dir, err := os.MkdirTemp("", "hflush")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &bufferedGuest{share: share, sock: filepath.Join(dir, "g.sock"), pending: map[string]string{}}
	l, err := net.Listen("unix", g.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "herd.ext.v1.SyncService",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Sync", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(new(herdext.SyncRequest)); err != nil {
				return nil, err
			}
			return g.sync(ctx)
		}}},
	}, g)
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	return g
}

// write is a process in the guest writing a file of a volume: it is not in the share yet.
func (g *bufferedGuest) write(rel, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending[rel] = body
}

func (g *bufferedGuest) sync(ctx context.Context) (*herdext.SyncResponse, error) {
	g.mu.Lock()
	g.syncs++
	g.mu.Unlock()
	if g.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if g.answer != nil {
		return nil, g.answer
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for rel, body := range g.pending {
		if err := os.WriteFile(filepath.Join(g.share, rel), []byte(body), 0o644); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		delete(g.pending, rel)
	}
	return &herdext.SyncResponse{}, nil
}

func (g *bufferedGuest) DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error) {
	if port != 8080 {
		return nil, errors.New("herd-guest serves on 8080")
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", g.sock)
}

// countingVMM counts the VMM snapshots it was asked for.
type countingVMM struct {
	*fake.Fake
	mu    sync.Mutex
	snaps int
}

func (c *countingVMM) Snapshot(ctx context.Context, id string, f vmm.Fidelity) (vmm.Snapshot, error) {
	c.mu.Lock()
	c.snaps++
	c.mu.Unlock()
	return c.Fake.Snapshot(ctx, id, f)
}

// flushHost is a host whose machines flush the guest through GuestFlush, the production client of herd-guest's Sync.
func flushHost(t *testing.T, caps vmm.Capabilities, guests func(share string) GuestDialer) (*countingVMM, *Machines) {
	t.Helper()
	dir := t.TempDir()
	v := &countingVMM{Fake: fake.New(caps, nil)}
	shares := filepath.Join(dir, "shares")
	cfg := Config{VMM: v, ShareDir: shares, Base: vmm.Spec{Kernel: "vmlinux", Initrd: "initramfs"}, FlushTimeout: 2 * time.Second,
		Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: registry(), Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}}
	if guests != nil {
		g := guests(filepath.Join(shares, "actor-1"))
		cfg.Guests, cfg.Flush = g, GuestFlush{Guests: g, Port: 8080}
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v, m
}

// TestSnapshotHoldsWhatTheGuestBuffered (PLT-195): a write the guest still buffers when the suspend comes is in the
// volume after the resume, at VOLUMES (cold boot from the bundle's tree) and at MEMORY, because the snapshot asks the
// guest to flush first.
func TestSnapshotHoldsWhatTheGuestBuffered(t *testing.T) {
	for _, f := range []vmm.Fidelity{vmm.FidelityVolumes, vmm.FidelityMemory} {
		t.Run(f.String(), func(t *testing.T) {
			ctx := context.Background()
			var g *bufferedGuest
			_, src := flushHost(t, ubuntuCaps, func(share string) GuestDialer { g = newBufferedGuest(t, share); return g })
			if err := src.Boot(ctx, "actor-1", template()); err != nil {
				t.Fatal(err)
			}
			g.write("data/conformance.txt", "before-suspend")
			var bundle bytes.Buffer
			if _, err := src.Snapshot(ctx, "actor-1", f, true, &bundle); err != nil {
				t.Fatal(err)
			}
			// A VOLUMES bundle cold-boots on another host; a MEMORY snapshot restores where it was taken.
			dst := src
			if f == vmm.FidelityVolumes {
				_, dst = flushHost(t, ubuntuCaps, nil)
			} else if p, err := src.Delete(ctx, "actor-1"); p != vmm.PresenceGone {
				// herd-api releases the suspended actor's VM, share included, before it resumes it.
				t.Fatalf("Delete after the suspend = %v, %v", p, err)
			}
			if err := dst.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dst.ShareDir("actor-1"), "data", "conformance.txt"))
			if err != nil || string(got) != "before-suspend" {
				t.Fatalf("data/conformance.txt after the resume: %q, %v; want what the guest wrote before the suspend", got, err)
			}
		})
	}
}

// TestSnapshotFailsWithoutAFlush: a guest that refuses the flush, never answers it, or cannot be reached fails the
// snapshot. The VMM is not asked for one, the VM keeps running, and no bundle is written.
func TestSnapshotFailsWithoutAFlush(t *testing.T) {
	for name, set := range map[string]func(*bufferedGuest) GuestDialer{
		"refused": func(g *bufferedGuest) GuestDialer {
			g.answer = status.Error(codes.Internal, "syncfs /mnt/volumes: input/output error")
			return g
		},
		"unanswered":  func(g *bufferedGuest) GuestDialer { g.hang = true; return g },
		"unreachable": func(g *bufferedGuest) GuestDialer { os.Remove(g.sock); return g },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			v, m := flushHost(t, ubuntuCaps, func(share string) GuestDialer { return set(newBufferedGuest(t, share)) })
			if err := m.Boot(ctx, "actor-1", template()); err != nil {
				t.Fatal(err)
			}
			var bundle bytes.Buffer
			start := time.Now()
			_, err := m.Snapshot(ctx, "actor-1", vmm.FidelityVolumes, true, &bundle)
			if err == nil || !strings.Contains(err.Error(), "flushing the guest") {
				t.Fatalf("Snapshot err = %v, want a failed flush", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Snapshot err %v reads as an unknown outcome; no snapshot was taken", err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("Snapshot took %v; the flush is bounded by FlushTimeout (2s)", d)
			}
			if v.snaps != 0 || bundle.Len() != 0 {
				t.Errorf("after a failed flush: %d VMM snapshots, %d bundle bytes; want none", v.snaps, bundle.Len())
			}
			if p, _ := v.Inspect(ctx, "actor-1"); p != vmm.PresencePresent {
				t.Errorf("after a failed flush the VM is %v, want it still running", p)
			}
		})
	}
}
