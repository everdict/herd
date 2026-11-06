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

// TestMemoryBundleLeavesOutZeroPages (DEFAUL-1601): a MEMORY bundle carries a memory file's pages that are zero as
// holes of a sparse tar entry, so a guest's untouched memory is neither stored nor moved; the bundle reads back as the
// same bytes through archive/tar, as every herdlet already reads bundles, so a restore needs no new reader.
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

// TestSparseMapStaysReadable: a memory file whose data runs are more than a sparse map may list (archive/tar reads
// at most 1 MiB of it) has the runs with the shortest gaps merged, zeros between them stored, and reads back whole.
func TestSparseMapStaysReadable(t *testing.T) {
	saved := maxFragments
	maxFragments = 100
	t.Cleanup(func() { maxFragments = saved })
	// 1000 one-page runs: 50 groups of 20 a page apart, the groups 32 pages apart.
	var runs [][2]int64
	for g := int64(0); g < 50; g++ {
		for i := int64(0); i < 20; i++ {
			runs = append(runs, [2]int64{(g*72 + 2*i) * 4096, 4096})
		}
	}
	f, want := memoryImage(t, 16<<20, runs)
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	frags, err := dataFragments(rc.(io.ReaderAt), f.Size)
	rc.Close()
	if err != nil || len(frags) > maxFragments || len(frags) < 2 {
		t.Fatalf("dataFragments = %d fragments, %v; want 2 to %d", len(frags), err, maxFragments)
	}
	var bundle bytes.Buffer
	if err := writeBundle(&bundle, Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&bundle)
	m, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	err = readMemory(tr, m, func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
		_, r, err := next()
		if err != nil {
			return err
		}
		got, err := io.ReadAll(r)
		if err == nil && !bytes.Equal(got, want) {
			t.Errorf("the memory file read back differs")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
