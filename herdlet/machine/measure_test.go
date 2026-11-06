package machine

import (
	"archive/tar"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
)

// TestMeasureMemoryBundle (DEFAUL-1601) measures, on a synthetic memory image, what a MEMORY bundle stores and the
// seconds per GiB its write and a restore's read of it take, with the memory file stored densely and as a sparse
// entry without its zero pages. A restore's read is what ImportSnapshot does: the memory file out of the bundle into a
// file, synced. It runs only when HERD_MEASURE is set (HERD_MEASURE_MIB, default 512; HERD_MEASURE_ZERO, default
// 0.85, the share of 1 MiB runs that are zero). Each line it logs starts with MEASURE.
func TestMeasureMemoryBundle(t *testing.T) {
	if os.Getenv("HERD_MEASURE") == "" {
		t.Skip("HERD_MEASURE is not set")
	}
	mib, zero := 512, 0.85
	if v, err := strconv.Atoi(os.Getenv("HERD_MEASURE_MIB")); err == nil {
		mib = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("HERD_MEASURE_ZERO"), 64); err == nil {
		zero = v
	}
	dir := t.TempDir()
	size := int64(mib) << 20
	gib := float64(size) / (1 << 30)
	path := filepath.Join(dir, "memory-ranges")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var coin [1]byte
	run := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(run)) {
		if _, err := rand.Read(coin[:]); err != nil {
			t.Fatal(err)
		}
		clear(run)
		if float64(coin[0])/256 >= zero {
			if _, err := rand.Read(run); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.Write(run); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	t.Logf("MEASURE image_bytes=%d zero_share_asked=%.2f", size, zero)
	for _, sparse := range []bool{false, true} {
		open := func() (io.ReadCloser, error) {
			f, err := os.Open(path)
			if err != nil || sparse {
				return f, err
			}
			return struct{ io.ReadCloser }{f}, nil // no ReadAt: written densely, as before
		}
		for i := range 3 {
			bpath := filepath.Join(dir, "bundle")
			b, err := os.Create(bpath)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			m := Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{{Name: "memory-ranges", Size: size, Open: open}}}
			if err := writeBundle(b, m, dir); err != nil {
				t.Fatal(err)
			}
			if err := b.Sync(); err != nil {
				t.Fatal(err)
			}
			write := time.Since(start)
			stored, _ := b.Seek(0, io.SeekEnd)
			if _, err := b.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "restored")
			start = time.Now()
			tr := tar.NewReader(b)
			got, err := readManifest(tr)
			if err != nil {
				t.Fatal(err)
			}
			err = readMemory(tr, got, func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
				f, r, err := next()
				if err != nil {
					return err
				}
				return restoreFile(out, r, f.Size)
			})
			read := time.Since(start)
			b.Close()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MEASURE sparse=%v run=%d bundle_bytes=%d write_s_per_gib=%.3f restore_read_s_per_gib=%.3f", sparse, i, stored, write.Seconds()/gib, read.Seconds()/gib)
			os.Remove(out)
			os.Remove(bpath)
		}
	}
}

// restoreFile is cloudhypervisor's writeFrom: exactly size bytes of r into a new file, synced.
func restoreFile(path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
