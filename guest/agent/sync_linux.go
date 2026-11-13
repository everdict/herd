//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// syncFS flushes the filesystem f is on and reports its write-back errors.
func syncFS(f *os.File) error { return unix.Syncfs(int(f.Fd())) }

// procDropCaches is the kernel's drop_caches knob; a test points it elsewhere.
var procDropCaches = "/proc/sys/vm/drop_caches"

// procCompactMemory has the kernel gather its free memory into whole blocks; balloonDevices matches the guest's
// virtio balloons. Tests point them elsewhere.
var (
	procCompactMemory = "/proc/sys/vm/compact_memory"
	balloonDevices    = "/sys/bus/virtio/drivers/virtio_balloon/virtio*"
)

// ReportSettle is how long dropCaches waits for the guest kernel to report the pages it just freed: the kernel starts
// reporting two seconds after a free (PAGE_REPORTING_DELAY) and then runs through its free lists in one pass.
var ReportSettle = 3 * time.Second

// dropCaches frees the kernel's clean page cache and its reclaimable dentries and inodes (DropCachesLevel). In a guest
// with a virtio balloon it then gathers the free memory into whole blocks, the unit the kernel reports, and waits
// ReportSettle, so the pages are back with the host (holes that read zero) before the snapshot is taken.
func dropCaches() error {
	if err := os.WriteFile(procDropCaches, []byte(DropCachesLevel), 0); err != nil {
		return err
	}
	if m, _ := filepath.Glob(balloonDevices); len(m) == 0 {
		return nil
	}
	// A kernel without compaction has no such file; the free blocks it has are reported all the same.
	_ = os.WriteFile(procCompactMemory, []byte("1"), 0)
	time.Sleep(ReportSettle)
	return nil
}
