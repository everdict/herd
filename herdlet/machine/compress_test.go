package machine

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/vmm"
)

// touchedMemory writes a memory file of size bytes in which every page holds 16 random bytes and zeros: 1 MiB of data
// in 256 MiB, and no page left whole zero, as a guest's memory looks after its pages were touched once.
func touchedMemory(t *testing.T, size int64) (vmm.SnapshotFile, []byte) {
	t.Helper()
	b := make([]byte, size)
	for p := int64(0); p < size; p += zeroPage {
		if _, err := rand.Read(b[p+64 : p+80]); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(t.TempDir(), "memory-ranges")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return vmm.SnapshotFile{Name: "memory-ranges", Size: size, Open: func() (io.ReadCloser, error) { return os.Open(p) }}, b
}

// readBackMemory reads the bundle's one memory file through readManifest and readMemory, as a restore does.
func readBackMemory(t *testing.T, bundle []byte) ([]byte, error) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(bundle))
	m, err := readManifest(tr)
	if err != nil {
		return nil, err
	}
	var read []byte
	err = readMemory(tr, m, func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
		f, r, err := next()
		if err != nil {
			return err
		}
		read = make([]byte, f.Size)
		_, err = io.ReadFull(r, read)
		return err
	})
	return read, err
}

// TestMemoryBundleIsCompressed (PLT-591): a MEMORY bundle stores its memory file compressed, so a 256 MiB memory file
// holding 1 MiB of data spread over every page (no page whole zero, so leaving out zero pages saves nothing) takes
// far less than its size: at most 16 MiB, where an uncompressed bundle holds over 200 MiB. It reads back as the same
// bytes.
func TestMemoryBundleIsCompressed(t *testing.T) {
	const size = 256 << 20
	f, want := touchedMemory(t, size)
	var bundle bytes.Buffer
	if err := writeBundle(&bundle, Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Logf("a %d-byte memory file with 1 MiB of data on every page: a %d-byte bundle", size, bundle.Len())
	if bundle.Len() > 16<<20 {
		t.Errorf("the bundle holds %d bytes for a %d-byte memory file holding 1 MiB of data, want at most %d", bundle.Len(), size, 16<<20)
	}
	got, err := readBackMemory(t, bundle.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the memory file read back differs")
	}
}

// TestAlteredMemoryIsRefused (PLT-591): a byte of a MEMORY bundle's memory file altered in the bundle makes the read
// fail; it never hands a restore other memory than the snapshot's.
func TestAlteredMemoryIsRefused(t *testing.T) {
	const size = 16 << 20
	f, _ := touchedMemory(t, size)
	var bundle bytes.Buffer
	if err := writeBundle(&bundle, Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b := bundle.Bytes()
	// The middle of the bundle is the memory file's stored bytes, however they are stored.
	b[len(b)/2] ^= 0x40
	if _, err := readBackMemory(t, b); err == nil {
		t.Errorf("a bundle with a byte of its memory file altered read back without an error")
	}
}
