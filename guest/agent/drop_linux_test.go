//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDropCachesFreesThePageCache (PLT-591): the drop after Sync's flush writes "3" to drop_caches, the clean page
// cache as well as the dentries and inodes, so the cached file bytes are free memory, which the guest kernel clears
// (vmm.ZeroFreedPages), and a MEMORY snapshot does not carry them. "2" (v0.1.23) frees the dentries and inodes alone.
func TestDropCachesFreesThePageCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "drop_caches")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := procDropCaches
	procDropCaches = p
	t.Cleanup(func() { procDropCaches = old })
	if err := dropCaches(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "3" {
		t.Errorf("drop_caches was written %q (%v), want \"3\": the page cache (1) and the dentries and inodes (2)", b, err)
	}
}
