package layer_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/everdict/herd/guest/layer"
)

// onHeader is the archive Write fills. The first time the archive written so far holds name (an entry's header),
// it runs do before Write goes on: a file removed there vanishes at a known point of the walk, as Postgres's
// postmaster.pid did during a golden's layer save (PLT-380).
type onHeader struct {
	bytes.Buffer
	name string
	do   func()
	done bool
}

func (w *onHeader) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if !w.done && bytes.Contains(w.Bytes(), []byte(w.name)) {
		w.done = true
		w.do()
	}
	return n, err
}

// entries reads an archive whole, failing on any malformed part, and answers each entry's body by name.
func entries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("the archive is malformed: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("the archive is malformed at %s: %v", h.Name, err)
		}
		out[h.Name] = string(body)
	}
}

func remove(t *testing.T, p string) func() {
	return func() {
		if err := os.RemoveAll(p); err != nil {
			t.Error(err)
		}
	}
}

// A file the walk listed and that is gone when Write reaches it is not part of the layer: the save goes on without
// it, whether it went before its lstat (Info), its directory's read (WalkDir) or its open (Open). Each removal runs
// as the header just before that point reaches the archive.
func TestWriteSkipsVanishedFile(t *testing.T) {
	for _, c := range []struct {
		name    string
		trigger string // the header whose writing removes gone
		gone    string
		want    []string // entries the archive still holds
		absent  []string // entries it must not hold
	}{
		// a-kept's header is written after b-gone was listed and before b-gone's lstat.
		{"info", "upper/a-kept", "b-gone", []string{"upper/a-kept", "upper/e-after"}, []string{"upper/b-gone"}},
		// d-gone/ is listed and its header written before WalkDir reads it.
		{"walkdir", "upper/d-gone/", "d-gone", []string{"upper/a-kept", "upper/e-after"}, []string{"upper/d-gone/inner"}},
		// c-open's own header is written before Write opens it.
		{"open", "upper/c-open", "c-open", []string{"upper/a-kept", "upper/e-after"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			upper := t.TempDir()
			write(t, filepath.Join(upper, "a-kept"), "kept", 0o644)
			write(t, filepath.Join(upper, "b-gone"), "pid", 0o600)
			write(t, filepath.Join(upper, "c-open"), "opened", 0o600)
			write(t, filepath.Join(upper, "d-gone", "inner"), "inner", 0o600)
			write(t, filepath.Join(upper, "e-after"), "after", 0o644)
			w := &onHeader{name: c.trigger, do: remove(t, filepath.Join(upper, c.gone))}
			if err := layer.Write(w, map[string]string{"upper": upper}, nil); err != nil {
				t.Fatalf("Write = %v; want the save to go on without the vanished %s", err, c.gone)
			}
			if !w.done {
				t.Fatalf("the walk never wrote %s's header", c.trigger)
			}
			got := entries(t, w.Bytes())
			for _, n := range c.want {
				if _, ok := got[n]; !ok {
					t.Errorf("the archive lacks %s", n)
				}
			}
			for _, n := range c.absent {
				if _, ok := got[n]; ok {
					t.Errorf("the archive holds the vanished %s", n)
				}
			}
			// A file opened before it vanished may be kept, but only whole.
			if body, ok := got["upper/c-open"]; ok && body != "opened" {
				t.Errorf("upper/c-open = %q; want it whole or not at all", body)
			}
		})
	}
}

// Only a vanished file is let through: a directory replaced by a file between its listing and its read fails the
// save (ENOTDIR), as any error other than ENOENT does.
func TestWriteFailsOnOtherWalkError(t *testing.T) {
	upper := t.TempDir()
	write(t, filepath.Join(upper, "d", "inner"), "inner", 0o600)
	d := filepath.Join(upper, "d")
	w := &onHeader{name: "upper/d/", do: func() {
		if err := os.RemoveAll(d); err != nil {
			t.Error(err)
		}
		write(t, d, "now a file", 0o600)
	}}
	err := layer.Write(w, map[string]string{"upper": upper}, nil)
	if !w.done {
		t.Fatal("the walk never wrote d's header")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Write = %v; want it to fail with ENOTDIR", err)
	}
}
