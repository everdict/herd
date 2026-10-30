// Package storetest is the snapstore.Store contract every implementation runs (disk, s3): Put answers only for bytes
// that are durable and match its digest, a fresh handle onto the same store reads them at once, Verify re-reads, and
// a failed or unknown write never answers success nor leaves an object Stat reports.
package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
)

// WriteFault is what the backing store does to the next writes.
type WriteFault int

const (
	// Honest stores and answers truthfully.
	Honest WriteFault = iota
	// RefuseWrites fails every write and stores nothing.
	RefuseWrites
	// LoseAnswers stores the bytes and loses the answer: the writer cannot know the outcome.
	LoseAnswers
	// CorruptWrites stores other bytes than it was given and answers success.
	CorruptWrites
)

// Harness opens stores onto one backing store.
type Harness struct {
	// Open returns a new handle onto the backing store (each call a separate client, as herd-api and a herdlet are).
	Open func(t *testing.T) snapstore.Store
	// Tamper, if set, changes the stored bytes of key behind the store's back.
	Tamper func(t *testing.T, key string)
	// Fault, if set, makes the backing store fail the next writes as f says (Honest restores it).
	Fault func(t *testing.T, f WriteFault)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// onlyReader hides any Seek of the reader it wraps.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// failingReader yields n bytes and then an error.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("storetest: the source broke")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	for i := range p {
		p[i] = 'x'
	}
	f.n -= len(p)
	return len(p), nil
}

func readAll(t *testing.T, s snapstore.Store, key string) []byte {
	t.Helper()
	rc, err := s.Open(ctxT(t), key)
	if err != nil {
		t.Fatalf("Open(%s): %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Open(%s): read: %v", key, err)
	}
	return b
}

func wantNotFound(t *testing.T, s snapstore.Store, key, after string) {
	t.Helper()
	if o, err := s.Stat(ctxT(t), key); !errors.Is(err, snapstore.ErrNotFound) {
		t.Errorf("Stat(%s) after %s = %+v, %v; want ErrNotFound", key, after, o, err)
	}
}

// Run runs the contract against h.
func Run(t *testing.T, h Harness) {
	t.Run("PutIsDurableAndVerified", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		data := []byte("memory and volumes")
		o, err := s.Put(ctx, "s1/a1/1", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		want := snapstore.Object{Key: "s1/a1/1", Digest: digestOf(data), Size: int64(len(data))}
		if o != want {
			t.Fatalf("Put = %+v, want %+v", o, want)
		}
		// Another client (herd-api, after a herdlet's Put) sees exactly what Put answered for, at once.
		other := h.Open(t)
		if got, err := other.Stat(ctx, o.Key); err != nil || got != want {
			t.Errorf("Stat from another handle = %+v, %v; want %+v", got, err, want)
		}
		if got, err := other.Verify(ctx, o.Key, o.Digest); err != nil || got != want {
			t.Errorf("Verify from another handle = %+v, %v", got, err)
		}
		if b := readAll(t, other, o.Key); !bytes.Equal(b, data) {
			t.Errorf("Open = %q, want %q", b, data)
		}
		if _, err := other.Verify(ctx, o.Key, digestOf([]byte("other"))); !errors.Is(err, snapstore.ErrDigestMismatch) {
			t.Errorf("Verify with another digest: %v, want ErrDigestMismatch", err)
		}
	})

	t.Run("PutReadsAnyReader", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
		o, err := s.Put(ctx, "big", onlyReader{bytes.NewReader(data)})
		if err != nil {
			t.Fatal(err)
		}
		if o.Size != int64(len(data)) || o.Digest != digestOf(data) {
			t.Fatalf("Put = %+v", o)
		}
		if b := readAll(t, h.Open(t), "big"); !bytes.Equal(b, data) {
			t.Errorf("Open: %d bytes, want %d", len(b), len(data))
		}
		// A seekable reader already part-read: Put takes what is left, as io.Copy would.
		r := bytes.NewReader([]byte("skip:rest"))
		if _, err := r.Seek(5, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if o, err := s.Put(ctx, "rest", r); err != nil || o.Digest != digestOf([]byte("rest")) {
			t.Errorf("Put of a part-read reader = %+v, %v", o, err)
		}
		if o, err := s.Put(ctx, "empty", strings.NewReader("")); err != nil || o.Size != 0 || o.Digest != digestOf(nil) {
			t.Errorf("Put empty = %+v, %v", o, err)
		}
	})

	t.Run("OverwriteReportsTheNewBytes", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		if _, err := s.Put(ctx, "k", strings.NewReader("old")); err != nil {
			t.Fatal(err)
		}
		o, err := s.Put(ctx, "k", strings.NewReader("newer"))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := h.Open(t).Verify(ctx, "k", digestOf([]byte("newer"))); err != nil || got != o {
			t.Errorf("Verify after overwrite = %+v, %v", got, err)
		}
	})

	t.Run("UnknownKeys", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		if _, err := s.Stat(ctx, "no/such"); !errors.Is(err, snapstore.ErrNotFound) {
			t.Errorf("Stat: %v", err)
		}
		if _, err := s.Verify(ctx, "no/such", digestOf(nil)); !errors.Is(err, snapstore.ErrNotFound) {
			t.Errorf("Verify: %v", err)
		}
		if rc, err := s.Open(ctx, "no/such"); !errors.Is(err, snapstore.ErrNotFound) {
			if rc != nil {
				rc.Close()
			}
			t.Errorf("Open: %v", err)
		}
		if err := s.Delete(ctx, "no/such"); err != nil {
			t.Errorf("Delete of a missing object: %v", err)
		}
	})

	t.Run("BadKeys", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		for _, k := range []string{"../x", "a/../b", "/abs", "", "a//b", "a/./b", "sp ace", "q?x"} {
			if _, err := s.Put(ctx, k, strings.NewReader("x")); !errors.Is(err, snapstore.ErrBadKey) {
				t.Errorf("Put(%q) = %v, want ErrBadKey", k, err)
			}
			if _, err := s.Stat(ctx, k); !errors.Is(err, snapstore.ErrBadKey) {
				t.Errorf("Stat(%q) = %v, want ErrBadKey", k, err)
			}
			if _, err := s.Verify(ctx, k, digestOf(nil)); !errors.Is(err, snapstore.ErrBadKey) {
				t.Errorf("Verify(%q) = %v, want ErrBadKey", k, err)
			}
			if rc, err := s.Open(ctx, k); !errors.Is(err, snapstore.ErrBadKey) {
				if rc != nil {
					rc.Close()
				}
				t.Errorf("Open(%q) = %v, want ErrBadKey", k, err)
			}
			if err := s.Delete(ctx, k); !errors.Is(err, snapstore.ErrBadKey) {
				t.Errorf("Delete(%q) = %v, want ErrBadKey", k, err)
			}
		}
	})

	t.Run("DeleteIsVerified", func(t *testing.T) {
		ctx := ctxT(t)
		s := h.Open(t)
		if _, err := s.Put(ctx, "d/1", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "d/1"); err != nil {
			t.Fatal(err)
		}
		wantNotFound(t, h.Open(t), "d/1", "Delete")
		if err := s.Delete(ctx, "d/1"); err != nil {
			t.Errorf("second Delete: %v", err)
		}
	})

	t.Run("FailedPutLeavesNothing", func(t *testing.T) {
		s := h.Open(t)
		// Over an older object too: a failed Put must not leave Stat reporting the old bytes as if they were its.
		for _, prior := range []bool{false, true} {
			if prior {
				if _, err := s.Put(ctxT(t), "f/cancel", strings.NewReader("old")); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Put(ctxT(t), "f/broken", strings.NewReader("old")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if o, err := s.Put(ctx, "f/cancel", strings.NewReader("x")); err == nil {
				t.Errorf("Put with a cancelled context = %+v, nil", o)
			}
			wantNotFound(t, h.Open(t), "f/cancel", "a cancelled Put")
			if o, err := s.Put(ctxT(t), "f/broken", &failingReader{n: 100 << 10}); err == nil {
				t.Errorf("Put of a source that broke = %+v, nil", o)
			}
			wantNotFound(t, h.Open(t), "f/broken", "a Put whose source broke")
		}
	})

	t.Run("VerifyReReads", func(t *testing.T) {
		if h.Tamper == nil {
			t.Skip("the harness cannot change stored bytes")
		}
		ctx := ctxT(t)
		s := h.Open(t)
		o, err := s.Put(ctx, "t/1", strings.NewReader("memory"))
		if err != nil {
			t.Fatal(err)
		}
		h.Tamper(t, "t/1")
		if got, err := h.Open(t).Verify(ctx, "t/1", o.Digest); !errors.Is(err, snapstore.ErrDigestMismatch) {
			t.Errorf("Verify of changed bytes = %+v, %v; want ErrDigestMismatch", got, err)
		}
	})

	t.Run("FailedOrUnknownWriteNeverAnswersSuccess", func(t *testing.T) {
		if h.Fault == nil {
			t.Skip("the harness cannot fail writes")
		}
		for _, c := range []struct {
			name  string
			fault WriteFault
		}{{"refused", RefuseWrites}, {"answer lost", LoseAnswers}, {"stored other bytes", CorruptWrites}} {
			t.Run(c.name, func(t *testing.T) {
				s := h.Open(t)
				key := "w/" + strings.ReplaceAll(c.name, " ", "-")
				if _, err := s.Put(ctxT(t), key, strings.NewReader("old")); err != nil {
					t.Fatal(err)
				}
				h.Fault(t, c.fault)
				o, err := s.Put(ctxT(t), key, strings.NewReader("snapshot bytes"))
				h.Fault(t, Honest)
				if err == nil {
					t.Errorf("Put = %+v, nil; a write that %s answered success", o, c.name)
				}
				wantNotFound(t, h.Open(t), key, "a write that "+c.name)
			})
		}
	})
}
