package disk_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/snapstore/storetest"
)

func TestPutVerifyDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.Put(ctx, "s1/a1/1", strings.NewReader("memory"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Size != 6 || !strings.HasPrefix(o.Digest, "sha256:") {
		t.Errorf("Put = %+v", o)
	}
	if got, err := s.Verify(ctx, "s1/a1/1", o.Digest); err != nil || got != o {
		t.Errorf("Verify = %+v, %v", got, err)
	}
	if _, err := s.Verify(ctx, "s1/a1/1", "sha256:00"); !errors.Is(err, snapstore.ErrDigestMismatch) {
		t.Errorf("Verify wrong digest: %v", err)
	}
	// Bytes changed under the store: Verify must notice.
	if err := os.WriteFile(filepath.Join(dir, "objects", "s1", "a1", "1"), []byte("MEMORY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, "s1/a1/1", o.Digest); !errors.Is(err, snapstore.ErrDigestMismatch) {
		t.Errorf("Verify tampered: %v", err)
	}
	rc, err := s.Open(ctx, "s1/a1/1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "MEMORY" {
		t.Errorf("Open = %q", b)
	}
	if err := s.Delete(ctx, "s1/a1/1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "s1/a1/1"); !errors.Is(err, snapstore.ErrNotFound) {
		t.Errorf("Stat after Delete: %v", err)
	}
	if err := s.Delete(ctx, "s1/a1/1"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	for _, k := range []string{"../x", "a/../b", "/abs", "", "a//b"} {
		if _, err := s.Put(ctx, k, strings.NewReader("x")); !errors.Is(err, snapstore.ErrBadKey) {
			t.Errorf("Put(%q) = %v, want ErrBadKey", k, err)
		}
	}
}

func TestFailedPutLeavesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := s.Put(ctx, "k", strings.NewReader("x")); err == nil {
		t.Fatal("Put with a cancelled context succeeded")
	}
	if _, err := s.Stat(context.Background(), "k"); !errors.Is(err, snapstore.ErrNotFound) {
		t.Errorf("Stat after failed Put: %v", err)
	}
}

// A NoSync store (test harnesses) keeps the same contract: what Put answered is what Verify and Open give back.
func TestNoSyncKeepsTheContract(t *testing.T) {
	ctx := context.Background()
	s, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.NoSync = true
	o, err := s.Put(ctx, "s1/a1/1", strings.NewReader("memory"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Verify(ctx, "s1/a1/1", o.Digest); err != nil || got != o {
		t.Fatalf("Verify = %+v, %v", got, err)
	}
	rc, err := s.Open(ctx, "s1/a1/1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, err := io.ReadAll(rc); err != nil || string(b) != "memory" {
		t.Fatalf("Open = %q, %v", b, err)
	}
}

// TestContract: the disk store keeps the snapshot store contract (the one snapstore/s3 runs too).
func TestContract(t *testing.T) {
	dir := t.TempDir()
	storetest.Run(t, storetest.Harness{
		Open: func(t *testing.T) snapstore.Store {
			s, err := disk.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		Tamper: func(t *testing.T, key string) {
			p := filepath.Join(dir, "objects", filepath.FromSlash(key))
			b, err := os.ReadFile(p)
			if err != nil || len(b) == 0 {
				t.Fatalf("tamper %s: %v", key, err)
			}
			b[0] ^= 0xff
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
}

// TestListAndAbortUploadsOnDisk (A-58): an object whose Put died between the object and its digest file is listed
// (Stat reports none, and the sweep must still see its bytes); a Put's temporary file is not an object. AbortUploads
// removes temporary files older than the cutoff and leaves a newer one (a Put still writing) and every object.
func TestListAndAbortUploadsOnDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "sp/actors/a/u/1", strings.NewReader("whole")); err != nil {
		t.Fatal(err)
	}
	objs := filepath.Join(dir, "objects", "sp", "actors", "a", "u")
	if err := os.WriteFile(filepath.Join(objs, "2"), []byte("no digest"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, young := filepath.Join(objs, ".tmp-old"), filepath.Join(objs, ".tmp-young")
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, day, day); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, "sp/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range got {
		keys = append(keys, fmt.Sprintf("%s:%d", o.Key, o.Size))
	}
	if want := "sp/actors/a/u/1:5 sp/actors/a/u/2:9"; strings.Join(keys, " ") != want {
		t.Errorf("List = %v, want %s", keys, want)
	}
	n, err := s.AbortUploads(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("AbortUploads = %d, %v; want 1", n, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stale temporary file: %v, want gone", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("the young temporary file: %v, want kept", err)
	}
	if got, err := s.List(ctx, "sp/"); err != nil || len(got) != 2 {
		t.Errorf("List after AbortUploads = %+v, %v; want both objects", got, err)
	}
}
