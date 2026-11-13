package machine

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// guestKernel is a fake host whose VM has a memory and a model of the guest kernel's use of it (PLT-591): a page a
// process or the page cache filled holds its bytes; a page freed holds them still, unless the guest reports its free
// pages and the VMM gives them back to the host (reporting), which leaves a hole that reads zero; the page cache is
// freed by a drop_caches write that names it (1). Reporting is what the backend does (vmm.FreePageReporting, as
// cloudhypervisor applies it) unless the test turns it off, and its flush
// writes what herd-guest's Sync writes (dropLevel, agent.DropCachesLevel by default). Its MEMORY snapshot carries the
// memory as one file, as Cloud Hypervisor's memory-ranges.
type guestKernel struct {
	*fake.Fake
	mu        sync.Mutex
	mem       []byte
	cache     [][2]int // the page cache's [offset, end)
	reporting bool
	dropLevel string
}

// fill has a process (or, with cache, the page cache) write n bytes at off: random bytes, which no codec makes smaller.
func (g *guestKernel) fill(t *testing.T, off, n int, cache bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cache {
		g.cache = append(g.cache, [2]int{off, off + n})
	}
	if _, err := rand.Read(g.mem[off : off+n]); err != nil {
		t.Fatal(err)
	}
}

// free frees [off, off+n): its bytes stay unless the guest reports the pages and the host takes them back.
func (g *guestKernel) free(off, n int) {
	if g.reporting {
		clear(g.mem[off : off+n])
	}
}

// Flush is herd-guest's Sync: its drop_caches write frees the page cache when it names it.
func (g *guestKernel) Flush(ctx context.Context, vmID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if lv := g.dropLevel; lv == "1" || lv == "3" {
		for _, r := range g.cache {
			g.free(r[0], r[1]-r[0])
		}
		g.cache = nil
	}
	return nil
}

func (g *guestKernel) ExportSnapshot(ctx context.Context, s vmm.Snapshot) ([]vmm.SnapshotFile, error) {
	g.mu.Lock()
	b := bytes.Clone(g.mem)
	g.mu.Unlock()
	return []vmm.SnapshotFile{{Name: "memory-ranges", Size: int64(len(b)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}}, nil
}

// TestFreedGuestMemoryLeavesTheBundle (PLT-591): a VM of 256 MiB whose processes hold 32 MiB, that filled and freed
// 96 MiB more (a build that exited) and read 96 MiB of files into its page cache, takes a MEMORY bundle of about the
// 32 MiB it holds: the guest reports its free pages (vmm.FreePageReporting), so freed pages are holes that read zero,
// and the flush drops the page cache ("3"), so the cache is freed and reported too; the bundle leaves zero pages out.
// With no reporting and a drop of "2" (v0.1.23) the freed bytes and the cache stay in the memory and the bundle
// carries them; that run is logged.
func TestFreedGuestMemoryLeavesTheBundle(t *testing.T) {
	const size, held, freed, cached = 256 << 20, 32 << 20, 96 << 20, 96 << 20
	run := func(t *testing.T, reporting bool, dropLevel string) int {
		dir := t.TempDir()
		g := &guestKernel{Fake: fake.New(ubuntuCaps, nil), mem: make([]byte, size), reporting: reporting, dropLevel: dropLevel}
		reg := registry()
		cache := &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: ubuntuCaps.Arch}}
		m, err := New(Config{VMM: g, Images: cache, ShareDir: filepath.Join(dir, "shares"), Flush: g,
			Base: vmm.Spec{Kernel: "vmlinux", Initrd: "initramfs"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := m.Boot(ctx, "a1", template()); err != nil {
			t.Fatal(err)
		}
		g.fill(t, 0, held, false)
		g.fill(t, held, freed, false)
		g.mu.Lock()
		g.free(held, freed)
		g.mu.Unlock()
		g.fill(t, held+freed, cached, true)
		var bundle bytes.Buffer
		if _, err := m.Snapshot(ctx, "a1", vmm.FidelityMemory, false, &bundle); err != nil {
			t.Fatal(err)
		}
		return bundle.Len()
	}
	got := run(t, vmm.FreePageReporting, agent.DropCachesLevel)
	t.Logf("a %d-byte VM holding %d bytes, %d freed, %d in its page cache: a %d-byte bundle", size, held, freed, cached, got)
	if limit := held + 4<<20; got > limit {
		t.Errorf("the bundle holds %d bytes, want at most %d: the VM holds %d bytes; its freed memory and page cache are in the bundle", got, limit, held)
	}
	t.Logf("with no free page reporting and drop_caches 2 (v0.1.23): a %d-byte bundle", run(t, false, "2"))
}
