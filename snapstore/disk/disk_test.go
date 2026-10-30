package disk_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
