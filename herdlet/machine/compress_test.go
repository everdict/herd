package machine

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/vmm"
	"github.com/pierrec/lz4/v4"
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

// rewriteBundle copies a bundle entry by entry, each entry's body through edit.
func rewriteBundle(t *testing.T, b []byte, edit func(name string, body []byte) []byte) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		body = edit(h.Name, body)
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestMemoryThatExpandsToOtherBytesIsRefused (PLT-591): a version 4 memory file carries its raw size and raw SHA-256
// (its end entry), and a restore refuses a file that expands to another size or digest before it hands over the
// file's last byte, as cloudhypervisor's import reads exactly the file's size: an end entry that records another
// digest or size, and a frame that expands to more or fewer bytes than its header says.
func TestMemoryThatExpandsToOtherBytesIsRefused(t *testing.T) {
	f, want := touchedMemory(t, 12<<20)
	var good bytes.Buffer
	if err := writeBundle(&good, Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, err := readBackMemory(t, good.Bytes()); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the unaltered bundle reads back %d bytes, %v", len(got), err)
	}
	end := bundleMemory + "/memory-ranges/" + frameEnd
	first := frameName("memory-ranges", 0)
	recompress := func(body []byte, raw func([]byte) []byte) []byte {
		b := make([]byte, binary.BigEndian.Uint64(body[8:frameHeader]))
		if err := decodeFrame(body[frameHeader:], b); err != nil {
			t.Fatal(err)
		}
		b = raw(b)
		var c lz4.Compressor
		stored, err := encodeFrame(&c, make([]byte, lz4.CompressBlockBound(len(b))), b)
		if err != nil {
			t.Fatal(err)
		}
		return append(bytes.Clone(body[:frameHeader]), stored...)
	}
	for name, edit := range map[string]func(string, []byte) []byte{
		"another digest": func(n string, b []byte) []byte {
			if n == end {
				b[len(b)-1] ^= 1
			}
			return b
		},
		"another size": func(n string, b []byte) []byte {
			if n == end {
				binary.BigEndian.PutUint64(b[:8], uint64(f.Size+zeroPage))
			}
			return b
		},
		"a frame that expands to fewer bytes": func(n string, b []byte) []byte {
			if n == first {
				return recompress(b, func(raw []byte) []byte { return raw[:len(raw)-1] })
			}
			return b
		},
		"a frame that expands to more bytes": func(n string, b []byte) []byte {
			if n == first {
				return recompress(b, func(raw []byte) []byte { return append(raw, 0) })
			}
			return b
		},
		"a frame's bytes altered, its header not": func(n string, b []byte) []byte {
			if n == first {
				return recompress(b, func(raw []byte) []byte { raw[100] ^= 1; return raw })
			}
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readBackMemory(t, rewriteBundle(t, bytes.Clone(good.Bytes()), edit)); err == nil {
				t.Errorf("the memory file read back whole")
			}
		})
	}
}

// TestIncompressibleMemoryIsStoredRaw (PLT-591): a frame LZ4 cannot make shorter (random bytes) is stored as its raw
// bytes, so a bundle of memory that does not compress is no larger than the memory and its frame headers, and it
// reads back as the same bytes.
func TestIncompressibleMemoryIsStoredRaw(t *testing.T) {
	const size = 8 << 20
	want := make([]byte, size)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "memory-ranges")
	if err := os.WriteFile(p, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f := vmm.SnapshotFile{Name: "memory-ranges", Size: size, Open: func() (io.ReadCloser, error) { return os.Open(p) }}
	var bundle bytes.Buffer
	if err := writeBundle(&bundle, Manifest{Version: bundleVersion, Memory: []vmm.SnapshotFile{f}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if limit := size + 64<<10; bundle.Len() > limit {
		t.Errorf("a %d-byte memory file of random bytes: a %d-byte bundle, want at most %d", size, bundle.Len(), limit)
	}
	got, err := readBackMemory(t, bundle.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the memory file read back differs")
	}
}
