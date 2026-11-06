package machine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// clockHost is a host whose VMs run the real herd-guest (fakeguest, each guest with a wall clock of its own) and whose
// machines hand a guest the host's clock through GuestClock, the production client of herd-guest's SetClock.
func clockHost(t *testing.T) (*fakeguest.VMM, *Machines) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hclk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v := fakeguest.New(fake.New(ubuntuCaps, nil), filepath.Join(dir, "vms"), nil)
	t.Cleanup(v.Close)
	m, err := New(Config{VMM: v, Guests: v, Clock: GuestClock{Guests: v, Port: fakeguest.GuestPort}, ClockTimeout: 5 * time.Second,
		ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux", Initrd: "initramfs"},
		Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: registry(), Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		t.Fatal(err)
	}
	return v, m
}

// onHostClock fails unless vm's guest reads the host's wall clock within a second.
func onHostClock(t *testing.T, v *fakeguest.VMM, vm, what string) {
	t.Helper()
	if d := time.Since(v.Clock(vm)); d > time.Second || d < -time.Second {
		t.Fatalf("%s: the guest's wall clock is %v behind the host's, want within a second", what, d.Round(time.Second))
	}
}

// TestRestoredGuestReadsTheHostsClock (DEFAUL-1595): a VM restored from a MEMORY snapshot reads the host's wall clock,
// not the one its snapshot's memory held, by the time Restore answers (herdlet lets nothing reach the VM before): from
// a golden built days ago, on the resume of a suspended actor, and as a fork of a running one.
func TestRestoredGuestReadsTheHostsClock(t *testing.T) {
	cases := []struct {
		name     string
		stop     bool // the snapshot is a suspend's
		age      time.Duration
		restored string
	}{
		{"golden", false, 72 * time.Hour, "actor-2"},
		{"resume", true, 12 * time.Minute, "actor-1"},
		{"fork", false, 20 * time.Second, "fork-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			v, m := clockHost(t)
			if err := m.Boot(ctx, "actor-1", template()); err != nil {
				t.Fatal(err)
			}
			var bundle bytes.Buffer
			if _, err := m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, c.stop, &bundle); err != nil {
				t.Fatal(err)
			}
			if c.stop {
				// herd-api releases a suspended actor's VM before it resumes it.
				if p, err := m.Delete(ctx, "actor-1"); p != vmm.PresenceGone {
					t.Fatal(p, err)
				}
			}
			v.AgeSnapshots(c.age)
			if err := m.Restore(ctx, c.restored, &bundle); err != nil {
				t.Fatal(err)
			}
			onHostClock(t, v, m.VMID(c.restored), "restored from a snapshot "+c.age.String()+" old")
		})
	}
}

// TestSnapshottedGuestRunsOnTheHostsClock (DEFAUL-1595): a VM a snapshot paused and left running (a golden's or a
// fork's source) runs on the host's clock after it, not behind by the pause.
func TestSnapshottedGuestRunsOnTheHostsClock(t *testing.T) {
	ctx := context.Background()
	v, m := clockHost(t)
	v.PauseLag = 20 * time.Second
	if err := m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	onHostClock(t, v, "actor-1", "after a snapshot that paused it for 20s")
}

// TestWarmVMIsHandedOutOnTheHostsClock (DEFAUL-1595): a warm VM whose clock drifted from the host's while it waited is
// handed out on the host's.
func TestWarmVMIsHandedOutOnTheHostsClock(t *testing.T) {
	ctx := context.Background()
	v, m := clockHost(t)
	tpl := template()
	tpl.Warm = 1
	p := &Pool{Machines: m, Health: GuestHealth{VMM: v, Guests: v, Port: fakeguest.GuestPort}}
	p.Set([]link.Template{tpl})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	ids := p.Counts()
	if ids[tpl.UID] != 1 {
		t.Fatalf("warm VMs %v, want one", ids)
	}
	p.mu.Lock()
	warm := p.ready[tpl.UID][0]
	p.mu.Unlock()
	v.Skew(warm, -5*time.Minute)
	id, ok := p.Take(ctx, tpl)
	if !ok || id != warm {
		t.Fatalf("took %q %v, want the warm VM %s", id, ok, warm)
	}
	onHostClock(t, v, id, "a warm VM handed out")
}

// failingClock answers every clock set with err.
type failingClock struct{ err error }

func (c failingClock) SetClock(context.Context, string) (time.Duration, error) { return 0, c.err }

// TestRestoreRefusesAGuestWhoseClockCannotBeSet (DEFAUL-1595): a MEMORY restore whose guest does not take the host's
// clock fails and leaves no VM, share or record behind, so no session runs on the snapshot's clock. A guest whose
// herd-guest predates ClockService (a snapshot taken before it) is restored and logged as left on its clock.
func TestRestoreRefusesAGuestWhoseClockCannotBeSet(t *testing.T) {
	ctx := context.Background()
	v, m := clockHost(t)
	if err := m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, false, &bundle); err != nil {
		t.Fatal(err)
	}
	b := bundle.Bytes()
	m.cfg.Clock = failingClock{errors.New("guest of fork-1 did not answer on port 8080: context deadline exceeded")}
	err := m.Restore(ctx, "fork-1", bytes.NewReader(b))
	if err == nil {
		t.Fatal("restored a guest whose clock could not be set")
	}
	if p, _ := v.Inspect(ctx, "fork-1"); p != vmm.PresenceGone {
		t.Fatalf("after the refused restore the VM is %v, want gone (%v)", p, err)
	}
	if _, serr := os.Stat(m.ShareDir("fork-1")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("after the refused restore its share is still there: %v", serr)
	}
	if m.Images("fork-1") != nil {
		t.Fatal("after the refused restore the VM is still recorded")
	}
	m.cfg.Clock = failingClock{ErrClockUnsupported}
	if err := m.Restore(ctx, "fork-1", bytes.NewReader(b)); err != nil {
		t.Fatalf("a guest that predates ClockService was refused: %v", err)
	}
}

// TestGuestClockOfAnOlderGuest: a herd-guest without ClockService answers ErrClockUnsupported, not a failure.
func TestGuestClockOfAnOlderGuest(t *testing.T) {
	g := newBufferedGuest(t, t.TempDir())
	_, err := GuestClock{Guests: g, Port: 8080}.SetClock(context.Background(), "actor-1")
	if !errors.Is(err, ErrClockUnsupported) {
		t.Fatalf("SetClock of a guest that serves only Sync: %v, want ErrClockUnsupported", err)
	}
}

// A restored guest's vsock accepts before its server answers: walked live, the first connection ended in "error reading
// server preface: EOF", and the restore was refused with a healthy guest. The call is tried again until its context ends.
func TestSetClockRetriesAGuestThatIsNotServingYet(t *testing.T) {
	calls := 0
	once := func() (time.Duration, error) {
		calls++
		if calls < 3 {
			return 0, status.Error(codes.Unavailable, "error reading server preface: EOF")
		}
		return time.Second, nil
	}
	got, err := retryUnavailable(context.Background(), once)
	if err != nil || got != time.Second || calls != 3 {
		t.Fatalf("retryUnavailable = %v, %v after %d calls", got, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if _, err := retryUnavailable(ctx, func() (time.Duration, error) {
		calls++
		return 0, status.Error(codes.Unavailable, "down")
	}); err == nil || calls != 1 {
		t.Fatalf("an ended context answered %v after %d calls", err, calls)
	}
	calls = 0
	if _, err := retryUnavailable(context.Background(), func() (time.Duration, error) {
		calls++
		return 0, ErrClockUnsupported
	}); !errors.Is(err, ErrClockUnsupported) || calls != 1 {
		t.Fatalf("a refusal answered %v after %d calls", err, calls)
	}
}
