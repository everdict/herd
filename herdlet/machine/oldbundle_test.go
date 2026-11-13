package machine

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/guest/layer"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// v3Fixture is a MEMORY bundle as bundle version 3 (herd v0.1.23) wrote it, kept as bytes: two memory files, one with
// zero pages (a sparse entry) and one dense, a volume with a file and a symlink, and a container's layer.
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

func v3Share(t *testing.T) string {
	t.Helper()
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "data", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "data", "hello.txt"), []byte("hello from v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../hello.txt", filepath.Join(share, "data", "sub", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(share, layer.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, layer.Dir, "agent"+layer.Ext), []byte("agent's layer"), 0o600); err != nil {
		t.Fatal(err)
	}
	return share
}

// TestWriteV3Fixture writes the fixture with the bundle writer of the moment; it runs only when HERD_WRITE_V3_FIXTURE
// is set, and was run once on bundle version 3.
func TestWriteV3Fixture(t *testing.T) {
	if os.Getenv("HERD_WRITE_V3_FIXTURE") == "" {
		t.Skip("set HERD_WRITE_V3_FIXTURE to write " + v3Fixture)
	}
	if bundleVersion != 3 {
		t.Fatalf("the fixture is of bundle version 3; this writer writes %d", bundleVersion)
	}
	dir := t.TempDir()
	var files []vmm.SnapshotFile
	for _, name := range []string{"memory-ranges", "state.json"} {
		b := v3Memory()[name]
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, vmm.SnapshotFile{Name: name, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return os.Open(p) }})
	}
	m := Manifest{Version: 3, Snapshot: vmm.Snapshot{ID: "snap-v3", VMID: "vm-v3", Fidelity: vmm.FidelityMemory, RestoreFamily: ubuntuCaps.RestoreFamily},
		Template: link.Template{UID: "tpl-v3", Volumes: []string{"data"}}, Platform: ubuntuCaps.Platform, Arch: ubuntuCaps.Arch, Memory: files}
	var b bytes.Buffer
	if err := writeBundle(&b, m, v3Share(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v3Fixture, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
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
