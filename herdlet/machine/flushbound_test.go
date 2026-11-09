package machine

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// slowFlush is a guest whose flush takes d (a guest saving a large writable layer), or never answers when d < 0.
type slowFlush struct{ d time.Duration }

func (f slowFlush) Flush(ctx context.Context, vmID string) error {
	if f.d < 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-time.After(f.d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// slowFlushHost boots actor-1 with memMiB of memory on a host whose guest flushes as f does.
func slowFlushHost(t *testing.T, f Flusher, timeout time.Duration, memMiB int) (*countingVMM, *Machines) {
	t.Helper()
	dir := t.TempDir()
	v := &countingVMM{Fake: fake.New(ubuntuCaps, nil)}
	m, err := New(Config{VMM: v, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux", Initrd: "initramfs"},
		Flush: f, FlushTimeout: timeout,
		Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: registry(), Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		t.Fatal(err)
	}
	tpl := template()
	tpl.MemoryMiB = memMiB
	if err := m.Boot(context.Background(), "actor-1", tpl); err != nil {
		t.Fatal(err)
	}
	return v, m
}

// The stated flush bound (PLT-378): 20 s plus the VM's memory at 64 MiB/s, so 116 s for a 6 GiB VM.
const (
	bigVMMiB      = 6 << 10
	bigVMFlushMax = 20*time.Second + bigVMMiB/64*time.Second
)

// TestLargeLayerFlushIsNotCutAt20s (PLT-378): a guest whose flush takes 40 s (a large writable layer saved to the
// share) on a 6 GiB VM is not failed at 20 s: the flush bound follows the VM's memory, and the snapshot is taken.
// Time is synctest's, so the test does not wait 40 s.
func TestLargeLayerFlushIsNotCutAt20s(t *testing.T) {
	v, m := slowFlushHost(t, slowFlush{d: 40 * time.Second}, 0, bigVMMiB)
	synctest.Test(t, func(t *testing.T) {
		var bundle bytes.Buffer
		start := time.Now()
		if _, err := m.Snapshot(context.Background(), "actor-1", vmm.FidelityMemory, true, &bundle); err != nil {
			t.Fatalf("Snapshot of a 6 GiB VM whose flush takes 40 s: %v (after %v); want the snapshot taken", err, time.Since(start))
		}
		if v.snaps != 1 || bundle.Len() == 0 {
			t.Errorf("after the flush: %d VMM snapshots, %d bundle bytes; want one snapshot and a bundle", v.snaps, bundle.Len())
		}
	})
}

// TestStuckGuestFailsTheSnapshotWithinItsBound (PLT-378): a guest that never answers the flush fails the snapshot at
// the stated bound for its memory (116 s for 6 GiB), or at Config.FlushTimeout when one is set. No VMM snapshot is
// taken, the VM keeps running, and the error is a failed flush, not an unknown outcome.
func TestStuckGuestFailsTheSnapshotWithinItsBound(t *testing.T) {
	for name, c := range map[string]struct {
		timeout, want time.Duration
	}{
		"bound from memory":     {0, bigVMFlushMax},
		"FlushTimeout override": {2 * time.Second, 2 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			v, m := slowFlushHost(t, slowFlush{d: -1}, c.timeout, bigVMMiB)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type result struct {
					err error
					d   time.Duration
				}
				done := make(chan result, 1)
				start := time.Now()
				go func() {
					_, err := m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, true, new(bytes.Buffer))
					done <- result{err, time.Since(start)}
				}()
				time.Sleep(c.want + time.Second)
				var r result
				select {
				case r = <-done:
				default:
					cancel()
					<-done
					t.Fatalf("a guest that never answers the flush: Snapshot still waiting after %v; want it failed at %v", c.want+time.Second, c.want)
				}
				if r.err == nil || !strings.Contains(r.err.Error(), "flushing the guest") {
					t.Fatalf("Snapshot err = %v, want a failed flush", r.err)
				}
				if r.d != c.want {
					t.Errorf("the flush failed after %v, want at the stated bound %v", r.d, c.want)
				}
				if v.snaps != 0 {
					t.Errorf("after a failed flush: %d VMM snapshots, want none", v.snaps)
				}
			})
			if p, _ := v.Inspect(context.Background(), "actor-1"); p != vmm.PresencePresent {
				t.Errorf("after a failed flush the VM is %v, want it still running", p)
			}
		})
	}
}
