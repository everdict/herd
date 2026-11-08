package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/layer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSyncFlushesEveryRootAndTheShare (PLT-195): Sync flushes each container's root and every SyncDir, one flush per
// directory, and answers a flush's error (or a directory that is not there) as Internal.
func TestSyncFlushesEveryRootAndTheShare(t *testing.T) {
	a, b := newTarget(t, "a"), newTarget(t, "b")
	g, err := agent.New(a, b)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	share := t.TempDir()
	g.SyncDirs = []string{share}
	var mu sync.Mutex
	var flushed []string
	g.SyncFS = func(f *os.File) error {
		mu.Lock()
		defer mu.Unlock()
		flushed = append(flushed, f.Name())
		return nil
	}
	if _, err := g.Sync(context.Background(), &herdext.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{a.Root, b.Root, share}
	sort.Strings(want)
	sort.Strings(flushed)
	if len(flushed) != len(want) {
		t.Fatalf("flushed %v, want %v", flushed, want)
	}
	for i := range want {
		if flushed[i] != want[i] {
			t.Fatalf("flushed %v, want %v", flushed, want)
		}
	}

	g.SyncFS = func(f *os.File) error {
		if f.Name() == share {
			return syscall.EIO
		}
		return nil
	}
	if _, err := g.Sync(context.Background(), &herdext.SyncRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("Sync with a failing flush = %v, want Internal", err)
	}
	g.SyncFS = nil
	g.SyncDirs = []string{filepath.Join(share, "gone")}
	if _, err := g.Sync(context.Background(), &herdext.SyncRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("Sync of a missing share = %v, want Internal", err)
	}
}

// TestSyncAnswersWhenTheCallerGivesUp: a flush that blocks (an unanswering virtio-fs server) does not hold the call
// past its deadline.
func TestSyncAnswersWhenTheCallerGivesUp(t *testing.T) {
	g, err := agent.New(newTarget(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	release := make(chan struct{})
	defer close(release)
	g.SyncFS = func(*os.File) error { <-release; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = g.Sync(ctx, &herdext.SyncRequest{})
	if status.Code(err) != codes.DeadlineExceeded || time.Since(start) > 2*time.Second {
		t.Fatalf("Sync with a blocked flush = %v after %v, want DeadlineExceeded at the deadline", err, time.Since(start))
	}
}

// TestSyncSavesEachContainersLayer (DEFAUL-1619): with a LayerShare, Sync saves each container's layer directories
// into it, without its exec records (a cold boot runs none of their processes); a target with no layers saves none.
func TestSyncSavesEachContainersLayer(t *testing.T) {
	a, b := newTarget(t, "a"), newTarget(t, "b")
	a.LayerDirs = map[string]string{"upper": a.Root}
	g, err := agent.New(a, b)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := os.MkdirAll(filepath.Join(a.Root, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Root, "root", "marker"), []byte("parent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.Root, "everdict", "exec", "p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	g.LayerShare = share
	g.SyncFS = func(*os.File) error { return nil }
	if _, err := g.Sync(context.Background(), &herdext.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, layer.Path("b"))); !os.IsNotExist(err) {
		t.Errorf("a target with no layers saved one: %v", err)
	}
	up := t.TempDir()
	if ok, err := layer.Restore(share, "a", map[string]string{"upper": up}); !ok || err != nil {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	if got, err := os.ReadFile(filepath.Join(up, "root", "marker")); err != nil || string(got) != "parent" {
		t.Errorf("root/marker = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(up, "everdict", "exec")); !os.IsNotExist(err) {
		t.Errorf("the exec records were saved: %v", err)
	}
}
