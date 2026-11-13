package machine

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/layer"
	"github.com/everdict/herd/link"
)

// text is n bytes a compressor shortens, as source files are.
func text(n int) []byte {
	line := []byte("export const answer = (question) => question.length > 0 ? 42 : undefined;\n")
	return bytes.Repeat(line, n/len(line)+1)[:n]
}

func noise(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// bundleOf writes the bundle of a share with one volume, w.
func bundleOf(t *testing.T, share string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := writeBundle(&b, Manifest{Version: bundleVersion, Template: link.Template{Volumes: []string{"w"}}}, share); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}
	return b.Bytes()
}

// restoreOf extracts a bundle's volumes and layers into a new share, as a restore does.
func restoreOf(t *testing.T, bundle []byte) (string, error) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(bundle))
	m, err := readManifest(tr)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if err := readMemory(tr, m, nil); err != nil {
		t.Fatalf("readMemory: %v", err)
	}
	out := t.TempDir()
	root, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return out, extractVolumes(tr, root, m.Template.Volumes)
}

func putFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVolumeFilesCostWhatCompressionLeaves (PLT-591): a delegate box's bundle was its memory plus its whole workspace,
// byte for byte (a 1.4 GB checkout with node_modules in a 2.7 GB bundle, measured on v0.1.26). The files of a volume
// and a container's layer archive are stored compressed, in pieces, and a restore gives every byte back: an empty
// file, a file of several pieces, a file compression does not shorten, and one that is both.
func TestVolumeFilesCostWhatCompressionLeaves(t *testing.T) {
	share := t.TempDir()
	mixed := append(text(pieceRaw+100), noise(t, pieceRaw)...) // a compressed piece, a raw piece, and a short last one
	mixed = append(mixed, text(300)...)
	files := map[string][]byte{
		"w/empty":                      {},
		"w/small.js":                   text(900),
		"w/src/three.js":               text(2*pieceRaw + 12345),
		"w/bin/noise":                  noise(t, pieceRaw+777),
		"w/bin/mixed":                  mixed,
		"w/exactly-a-piece":            text(pieceRaw),
		layer.Dir + "/box" + layer.Ext: text(pieceRaw + 5000),
	}
	raw, compressible := 0, 0
	for name, b := range files {
		putFile(t, filepath.Join(share, name), b)
		raw += len(b)
		if !strings.Contains(name, "bin/") {
			compressible += len(b)
		}
	}
	bundle := bundleOf(t, share)
	// What does not compress is noise's and mixed's random bytes; the text must cost a small part of itself.
	if got, most := len(bundle), raw-compressible+compressible/10+pieceRaw; got > most {
		t.Fatalf("the bundle of %d raw bytes (%d of them text) is %d bytes, want at most %d: the files are not stored compressed", raw, compressible, got, most)
	}
	out, err := restoreOf(t, bundle)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: restored %d bytes that differ from the %d written", name, len(got), len(want))
		}
	}
}

// TestHardLinksAreOneFile (PLT-591): a file with several names in a volume (a package store linked into node_modules)
// was written once per name and restored as separate files. It is stored once, and a restore links its names again.
func TestHardLinksAreOneFile(t *testing.T) {
	share := t.TempDir()
	body := noise(t, 1<<20)
	putFile(t, filepath.Join(share, "w/store/pkg"), body)
	for _, name := range []string{"w/node_modules/a/pkg", "w/node_modules/b/pkg"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(share, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(share, "w/store/pkg"), filepath.Join(share, name)); err != nil {
			t.Skipf("no hard links here: %v", err)
		}
	}
	putFile(t, filepath.Join(share, "w/alone"), []byte("one name"))
	bundle := bundleOf(t, share)
	if len(bundle) > len(body)+len(body)/2 {
		t.Fatalf("the bundle of one %d-byte file under three names is %d bytes: each name was written as a file", len(body), len(bundle))
	}
	out, err := restoreOf(t, bundle)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	store, err := os.Stat(filepath.Join(out, "w/store/pkg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"w/node_modules/a/pkg", "w/node_modules/b/pkg"} {
		fi, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !os.SameFile(store, fi) {
			t.Fatalf("%s is restored as a file of its own, not a name of w/store/pkg", name)
		}
		got, _ := os.ReadFile(filepath.Join(out, name))
		if !bytes.Equal(got, body) {
			t.Fatalf("%s: restored bytes differ", name)
		}
	}
	alone, err := os.Stat(filepath.Join(out, "w/alone"))
	if err != nil || os.SameFile(store, alone) {
		t.Fatalf("w/alone: %v, same file as the store's: %v", err, err == nil)
	}
}

// editBundle copies a bundle header by header through edit, which may change a header (and its body's bytes).
func editBundle(t *testing.T, b []byte, edit func(h *tar.Header, body []byte) []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		body = edit(h, body)
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

// TestAlteredPiecesAndLinksAreRefused (PLT-591): a restore never writes a file other than the one the bundle's writer
// read, nor a name outside the volumes: a piece out of order, a piece that claims another raw length, and a link to a
// file outside the declared volumes are each refused.
func TestAlteredPiecesAndLinksAreRefused(t *testing.T) {
	share := t.TempDir()
	putFile(t, filepath.Join(share, "w/big.js"), text(2*pieceRaw+10))
	putFile(t, filepath.Join(share, "w/a"), noise(t, 4096))
	if err := os.Link(filepath.Join(share, "w/a"), filepath.Join(share, "w/b")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	good := bundleOf(t, share)
	if _, err := restoreOf(t, good); err != nil {
		t.Fatalf("the unaltered bundle: %v", err)
	}
	cases := map[string]func(h *tar.Header, body []byte) []byte{
		"a piece out of order": func(h *tar.Header, body []byte) []byte {
			if h.PAXRecords[paxPart] == "1" {
				h.PAXRecords[paxPart] = "2"
			}
			return body
		},
		"a further piece of another file": func(h *tar.Header, body []byte) []byte {
			if h.PAXRecords[paxPart] == "1" {
				h.Name = bundleVolumes + "/w/other.js"
			}
			return body
		},
		"a raw length the piece does not expand to": func(h *tar.Header, body []byte) []byte {
			if h.Name == bundleVolumes+"/w/big.js" && h.PAXRecords[paxPart] == "" {
				h.PAXRecords[paxRaw] = "4096"
			}
			return body
		},
		"a raw length over a piece": func(h *tar.Header, body []byte) []byte {
			if h.Name == bundleVolumes+"/w/big.js" && h.PAXRecords[paxPart] == "" {
				h.PAXRecords[paxRaw] = "999999999"
			}
			return body
		},
		"a link out of the volumes": func(h *tar.Header, body []byte) []byte {
			if h.Typeflag == tar.TypeLink {
				h.Linkname = bundleVolumes + "/w/../../etc/passwd"
			}
			return body
		},
		"a link into no declared volume": func(h *tar.Header, body []byte) []byte {
			if h.Typeflag == tar.TypeLink {
				h.Linkname = bundleVolumes + "/elsewhere/a"
			}
			return body
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			altered := editBundle(t, good, edit)
			if bytes.Equal(altered, good) {
				t.Fatal("the case altered nothing")
			}
			if _, err := restoreOf(t, altered); err == nil {
				t.Fatal("the altered bundle restored")
			}
		})
	}
}

// TestMeasureVolumeTree (PLT-591) writes the bundle of a real tree (HERD_MEASURE_TREE, say a checkout with its
// node_modules) and logs its raw bytes, the bundle's bytes and the seconds. It runs only when the variable is set.
func TestMeasureVolumeTree(t *testing.T) {
	tree := os.Getenv("HERD_MEASURE_TREE")
	if tree == "" {
		t.Skip("HERD_MEASURE_TREE is not set")
	}
	share := t.TempDir()
	if err := os.Symlink(tree, filepath.Join(share, "w")); err != nil {
		t.Fatal(err)
	}
	// The volume is the tree itself: writeBundle does not follow a symlinked volume, so walk its target.
	real, err := filepath.EvalSymlinks(tree)
	if err != nil {
		t.Fatal(err)
	}
	var raw, files int64
	_ = filepath.WalkDir(real, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				raw += fi.Size()
				files++
			}
		}
		return nil
	})
	n := &countWriter{}
	begun := time.Now()
	if err := writeBundle(n, Manifest{Version: bundleVersion, Template: link.Template{Volumes: []string{filepath.Base(real)}}}, filepath.Dir(real)); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}
	t.Logf("MEASURE tree=%s files=%d raw_bytes=%d bundle_bytes=%d seconds=%.1f", tree, files, raw, n.n, time.Since(begun).Seconds())
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }
