//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDropCachesFreesThePageCache (PLT-591): the drop after Sync's flush writes "3" to drop_caches, the clean page
// cache as well as the dentries and inodes, so the cached file bytes are free memory. "2" (v0.1.23) frees the dentries
// and inodes alone. A guest with no balloon is not made to wait.
func TestDropCachesFreesThePageCache(t *testing.T) {
	dir := t.TempDir()
	point(t, dir, "none*")
	start := time.Now()
	if err := dropCaches(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(procDropCaches); err != nil || string(b) != "3" {
		t.Errorf("drop_caches was written %q (%v), want \"3\": the page cache (1) and the dentries and inodes (2)", b, err)
	}
	if _, err := os.Stat(procCompactMemory); err == nil {
		t.Error("a guest with no balloon was compacted: nothing reports the blocks")
	}
	if d := time.Since(start); d >= ReportSettle {
		t.Errorf("the drop took %v in a guest with no balloon, want less than the settle of %v", d, ReportSettle)
	}
}

// TestDropCachesWaitsForTheReport (PLT-591): in a guest with a virtio balloon the drop gathers the free memory
// (compact_memory) and waits ReportSettle, so the freed pages are reported, and back with the host, before herdlet
// takes the snapshot. Without the wait the snapshot is taken inside the kernel's two-second reporting delay and
// carries the bytes the page cache held.
func TestDropCachesWaitsForTheReport(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "virtio3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	point(t, dir, "virtio*")
	start := time.Now()
	if err := dropCaches(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(procCompactMemory); err != nil || string(b) != "1" {
		t.Errorf("compact_memory was written %q (%v), want \"1\"", b, err)
	}
	if d := time.Since(start); d < ReportSettle {
		t.Errorf("the drop answered after %v, want at least the settle of %v", d, ReportSettle)
	}
}

// point has dropCaches write its knobs under dir, find its balloons by dir/glob, and settle in 50 ms.
func point(t *testing.T, dir, glob string) {
	t.Helper()
	oldDrop, oldCompact, oldBalloon, oldSettle := procDropCaches, procCompactMemory, balloonDevices, ReportSettle
	procDropCaches, procCompactMemory = filepath.Join(dir, "drop_caches"), filepath.Join(dir, "compact_memory")
	balloonDevices, ReportSettle = filepath.Join(dir, glob), 50*time.Millisecond
	t.Cleanup(func() {
		procDropCaches, procCompactMemory, balloonDevices, ReportSettle = oldDrop, oldCompact, oldBalloon, oldSettle
	})
}
