package layer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/guest/layer"
)

// TestSavedLayerIsCompressed (PLT-591): a container's layer is saved compressed, so 16 MiB of files written as a
// build or a package install writes them (text) take a fraction of an archive; an uncompressed tar (v0.1.23) holds
// them whole. It restores to the same files.
func TestSavedLayerIsCompressed(t *testing.T) {
	upper, share := t.TempDir(), t.TempDir()
	var body strings.Builder
	for i := 0; body.Len() < 1<<20; i++ {
		body.WriteString("export function handler" + strings.Repeat("x", i%7) + "(req, res) { return res.json({ ok: true }); }\n")
	}
	for i := range 16 {
		write(t, filepath.Join(upper, "node_modules", "pkg", string(rune('a'+i))+".js"), body.String(), 0o644)
	}
	if err := layer.Save(share, "main", map[string]string{"upper": upper}, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(share, layer.Path("main")))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("16 files of %d bytes: a %d-byte archive", body.Len(), fi.Size())
	if limit := int64(2 << 20); fi.Size() > limit {
		t.Errorf("the archive holds %d bytes for 16 MiB of text, want at most %d", fi.Size(), limit)
	}
	up2 := t.TempDir()
	if ok, err := layer.Restore(share, "main", map[string]string{"upper": up2}); err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	if got := read(t, filepath.Join(up2, "node_modules", "pkg", "p.js")); got != body.String() {
		t.Errorf("a restored file differs: %d bytes, want %d", len(got), body.Len())
	}
}

// TestUncompressedLayerStillRestores (PLT-591): an archive saved by herd v0.1.23 or before, a plain tar, which version 3
// bundles carry, still restores.
func TestUncompressedLayerStillRestores(t *testing.T) {
	upper, share := t.TempDir(), t.TempDir()
	write(t, filepath.Join(upper, "etc", "kept"), "from v0.1.23", 0o644)
	if err := os.MkdirAll(filepath.Join(share, layer.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(share, layer.Path("main")))
	if err != nil {
		t.Fatal(err)
	}
	if err := layer.Write(f, map[string]string{"upper": upper}, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	up2 := t.TempDir()
	if ok, err := layer.Restore(share, "main", map[string]string{"upper": up2}); err != nil || !ok {
		t.Fatalf("Restore of a plain tar = %v, %v", ok, err)
	}
	if got := read(t, filepath.Join(up2, "etc", "kept")); got != "from v0.1.23" {
		t.Errorf("etc/kept = %q", got)
	}
}
