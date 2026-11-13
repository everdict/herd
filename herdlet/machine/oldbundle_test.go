package machine

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/guest/layer"
	"github.com/everdict/herd/vmm"
)

// v3Fixture is a MEMORY bundle as bundle version 3 (herd v0.1.23) wrote it, kept as bytes: two memory files, one with
// zero pages (a sparse entry) and one dense, a volume with a file and a symlink, and a container's layer. It was
// written by the v0.1.23 writer (writeBundle at bundle version 3, test TestWriteV3Fixture of commit 8058598), which
// version 4 replaced.
const v3Fixture = "testdata/bundle-v3.tar"

// v3Memory is the content of the fixture's memory files, made again here byte for byte.
func v3Memory() map[string][]byte {
	ranges := make([]byte, 1<<20)
	for i := 3 * zeroPage; i < 5*zeroPage; i++ {
		ranges[i] = byte(i * 7)
	}
	for i := len(ranges) - zeroPage; i < len(ranges); i++ {
		ranges[i] = byte(i ^ 0x5a)
	}
	return map[string][]byte{"memory-ranges": ranges, "state.json": []byte(`{"vm":"v3","devices":["virtio-net","virtio-fs"]}`)}
}

// TestV3BundleStillRestores (PLT-591): a version 3 bundle, written by herd v0.1.23 and kept as bytes, reads back with
// today's reader byte for byte: its memory files, its volume tree and its layer.
func TestV3BundleStillRestores(t *testing.T) {
	b, err := os.ReadFile(v3Fixture)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	m, err := readManifest(tr)
	if err != nil {
		t.Fatalf("the v3 bundle's manifest: %v", err)
	}
	if m.Version != 3 || len(m.Memory) != 2 {
		t.Fatalf("manifest version %d with %d memory files, want 3 with 2", m.Version, len(m.Memory))
	}
	want := v3Memory()
	got := map[string][]byte{}
	err = readMemory(tr, m, func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
		for {
			f, r, err := next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			buf := make([]byte, f.Size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return err
			}
			got[f.Name] = buf
		}
	})
	if err != nil {
		t.Fatalf("the v3 bundle's memory files: %v", err)
	}
	for name, w := range want {
		if !bytes.Equal(got[name], w) {
			t.Errorf("memory file %s read back differs (%d bytes, want %d)", name, len(got[name]), len(w))
		}
	}
	share := t.TempDir()
	root, err := os.OpenRoot(share)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := extractVolumes(tr, root, m.Template.Volumes); err != nil {
		t.Fatalf("the v3 bundle's volumes: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(share, "data", "hello.txt")); err != nil || string(b) != "hello from v3\n" {
		t.Errorf("data/hello.txt = %q, %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(share, "data", "sub", "link")); err != nil || l != "../hello.txt" {
		t.Errorf("data/sub/link -> %q, %v", l, err)
	}
	if b, err := os.ReadFile(filepath.Join(share, layer.Dir, "agent"+layer.Ext)); err != nil || string(b) != "agent's layer" {
		t.Errorf("the layer = %q, %v", b, err)
	}
}
