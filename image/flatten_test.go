package image_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// TestFlattenCases pins Flatten's whole output, bytes included, for the paths a change to its tree could break.
func TestFlattenCases(t *testing.T) {
	type F = imagetest.File
	for _, c := range []struct {
		name   string
		layers [][]byte
	}{
		{"whiteout-implicit-parent", [][]byte{
			imagetest.Layer(
				F{Name: "a/b/c/", Type: tar.TypeDir, Mode: 0o700},
				F{Name: "a/b/c/f", Body: "f"},
				F{Name: "a/b/c/g/h", Body: "h"},
				F{Name: "a/b/cc", Body: "cc"},
				F{Name: "a/b/c.d", Body: "c.d"},
				F{Name: "p/q/r", Body: "r"},
			),
			imagetest.Layer(
				F{Name: "a/b/.wh.c"},
				F{Name: "p/.wh.q"},
				F{Name: "nowhere/.wh.x"},
			),
		}},
		{"opaque", [][]byte{
			imagetest.Layer(
				F{Name: "o/", Type: tar.TypeDir, Mode: 0o750},
				F{Name: "o/a", Body: "a"},
				F{Name: "o/sub/b", Body: "b"},
				F{Name: "oo/k", Body: "k"},
				F{Name: "m/n/x", Body: "x"},
				F{Name: "keep", Body: "keep"},
			),
			imagetest.Layer(
				F{Name: "o/d", Body: "d"},
				F{Name: "o/.wh..wh..opq"},
				F{Name: "o/c", Body: "c"},
				F{Name: "m/.wh..wh..opq"},
			),
		}},
		{"opaque-root", [][]byte{
			imagetest.Layer(
				F{Name: "a/b", Body: "b"},
				F{Name: "c", Body: "c"},
			),
			imagetest.Layer(
				F{Name: "d/e", Body: "e"},
				F{Name: ".wh..wh..opq"},
				F{Name: "f", Body: "f"},
			),
		}},
		{"hardlink-replaced", [][]byte{
			imagetest.Layer(
				F{Name: "f", Body: "one", Mode: 0o600, UID: 5, GID: 6},
				F{Name: "l1", Type: tar.TypeLink, Link: "f"},
				F{Name: "d/l2", Type: tar.TypeLink, Link: "/f"},
				F{Name: "g", Body: "g"},
				F{Name: "gl", Type: tar.TypeLink, Link: "g"},
				F{Name: "h", Body: "h"},
				F{Name: "hl", Type: tar.TypeLink, Link: "h"},
			),
			imagetest.Layer(
				F{Name: "f", Body: "two", Mode: 0o644},
				F{Name: "l3", Type: tar.TypeLink, Link: "f"},
				F{Name: ".wh.gl"},
				F{Name: "g", Body: "g2"},
				F{Name: ".wh.h"},
			),
			imagetest.Layer(
				F{Name: "f", Body: "three"},
				F{Name: "gl", Type: tar.TypeLink, Link: "g"},
			),
		}},
		{"dir-replaced-by-file", [][]byte{
			imagetest.Layer(
				F{Name: "d/", Type: tar.TypeDir},
				F{Name: "d/x", Body: "x"},
				F{Name: "d/e/y", Body: "y"},
				F{Name: "i/z", Body: "z"},
				F{Name: "k/z", Body: "z"},
				F{Name: "s/", Type: tar.TypeDir},
				F{Name: "s/t", Body: "t"},
			),
			imagetest.Layer(
				F{Name: "d", Body: "now a file"},
				F{Name: "i", Body: "now a file"},
				F{Name: "k/", Type: tar.TypeDir, Mode: 0o700},
				F{Name: "s", Type: tar.TypeSymlink, Link: "d"},
			),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := image.Flatten(readers(c.layers), &out, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(out.Bytes())
			l, err := image.Listing(&out)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "flatten-"+c.name+".golden", fmt.Sprintf("%ssha256:%x\n", l, sum))
		})
	}
}

// TestFlattenLinear flattens an image of 200,000 files, none replaced. Linear work does it in a few seconds; work
// per entry that grows with the tree (200,000²/2 path comparisons) takes hours, so the bound below separates the two
// with room for a slow or loaded machine.
func TestFlattenLinear(t *testing.T) {
	const (
		dirs, perDir = 200, 1000
		bound        = 60 * time.Second
	)
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for d := 0; d < dirs; d++ {
			for f := 0; f < perDir; f++ {
				h := &tar.Header{Name: fmt.Sprintf("usr/lib/d%03d/f%04d", d, f), Typeflag: tar.TypeReg, Mode: 0o644,
					Size: 1, ModTime: time.Unix(1700000000, 0)}
				if err := tw.WriteHeader(h); err != nil {
					pw.CloseWithError(err)
					return
				}
				if _, err := tw.Write([]byte{'x'}); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
		}
		pw.CloseWithError(tw.Close())
	}()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- image.Flatten([]io.Reader{pr}, io.Discard, t.TempDir()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("flattened %d files in %v", dirs*perDir, time.Since(start))
	case <-time.After(bound):
		t.Fatalf("flattening %d files took longer than %v: Flatten's work per entry grows with the tree", dirs*perDir, bound)
	}
}
