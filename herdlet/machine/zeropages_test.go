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

// memoryImage writes a memory file of size bytes, zero but for the given [offset, offset+length) runs of random
// bytes, and returns it as a vmm.SnapshotFile with its bytes.
func memoryImage(t *testing.T, size int64, runs [][2]int64) (vmm.SnapshotFile, []byte) {
	t.Helper()
	b := make([]byte, size)
	for _, r := range runs {
		if _, err := rand.Read(b[r[0] : r[0]+r[1]]); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(t.TempDir(), "memory-ranges")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return vmm.SnapshotFile{Name: "memory-ranges", Size: size, Open: func() (io.ReadCloser, error) { return os.Open(p) }}, b
}

// TestMemoryBundleLeavesOutZeroPages (DEFAUL-1601, PLT-591): a MEMORY bundle leaves a memory file's pages that are
// zero out of its frames, so a guest's untouched memory is neither stored nor moved; the bundle reads back as the same
// bytes.
func TestMemoryBundleLeavesOutZeroPages(t *testing.T) {
	const size = 64 << 20
	cases := []struct {
		name string
		runs [][2]int64
		max  int64 // the most the bundle may hold
	}{
		{"mostly zero", [][2]int64{{0, 4096}, {1 << 20, 3 * 4096}, {40 << 20, 1 << 20}, {size - 4096, 4096}}, 2 << 20},
		{"all zero", nil, 64 << 10},
		{"ends in a hole", [][2]int64{{8192, 4096}}, 64 << 10},
		{"no zero page", [][2]int64{{0, size}}, size + 64<<10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, want := memoryImage(t, size, c.runs)
			m := Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}
			var bundle bytes.Buffer
			if err := writeBundle(&bundle, m, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if int64(bundle.Len()) > c.max {
				t.Errorf("the bundle holds %d bytes for a %d-byte memory file, want at most %d", bundle.Len(), size, c.max)
			}
			tr := tar.NewReader(&bundle)
			got, err := readManifest(tr)
			if err != nil {
				t.Fatal(err)
			}
			var read []byte
			err = readMemory(tr, got, func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
				_, r, err := next()
				if err != nil {
					return err
				}
				read, err = io.ReadAll(r)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(read, want) {
				t.Errorf("the memory file read back differs (%d bytes, want %d)", len(read), len(want))
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Errorf("after the memory file: %v, want the end of the bundle", err)
			}
		})
	}
}
