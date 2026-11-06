package s3_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// copyFixture is an endpoint holding one source object, "src", and a store onto it: data is put as one PUT, or, with
// partSize set, as a multipart upload in parts of that size.
type copyFixture struct {
	srv  *s3test.Server
	s    *s3.Store
	data []byte
	src  snapstore.Object
}

func newCopyFixture(t *testing.T, size int, partSize int64) *copyFixture {
	t.Helper()
	srv := s3test.New("us-east-1", creds)
	srv.SetMinPartSize(0)
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey},
		&http.Client{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.PartSize = partSize
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	src, err := s.Put(t.Context(), "src", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return &copyFixture{srv: srv, s: s, data: data, src: src}
}

// counts are the object bytes that passed through the client so far: what the endpoint served
// in GETs and took in PUTs.
func (f *copyFixture) counts() (served, received int64) { return f.srv.Served(), f.srv.Received() }

func (f *copyFixture) wantCopy(t *testing.T, key string) {
	t.Helper()
	sum := sha256.Sum256(f.data)
	want := snapstore.Object{Key: key, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(f.data))}
	if got, err := f.s.Stat(t.Context(), key); err != nil || got != want {
		t.Fatalf("Stat %s = %+v, %v; want %+v", key, got, err, want)
	}
	rc, err := f.s.Open(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, err := io.ReadAll(rc); err != nil || !bytes.Equal(b, f.data) {
		t.Fatalf("%s reads %d bytes (%v), not the source's %d", key, len(b), err, len(f.data))
	}
}

var copyShapes = []struct {
	name     string
	size     int
	partSize int64
}{
	{"one PUT", 1 << 20, 0},
	{"multipart", 1<<20 + 17, 256 << 10},
}

// TestCopyIsServerSide (DEFAUL-1610): a copy within the bucket is a CopyObject, and no byte of the object passes
// through the client, neither to copy it nor to prove the copy: the endpoint's SHA-256 of the copy, read by a HEAD,
// is the source's digest. Its counterexample is the copy before DEFAUL-1610, which streamed the object out and back
// (and re-read it), as CreateTag of a suspended actor did for 13 minutes on a built box.
func TestCopyIsServerSide(t *testing.T) {
	for _, c := range copyShapes {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t, c.size, c.partSize)
			served, received := f.counts()
			got, err := f.s.Copy(t.Context(), "src", "dst", f.src.Digest)
			if want := (snapstore.Object{Key: "dst", Digest: f.src.Digest, Size: f.src.Size}); err != nil || got != want {
				t.Fatalf("Copy = %+v, %v; want %+v", got, err, want)
			}
			s, r := f.counts()
			if s != served || r != received {
				t.Errorf("the copy moved %d bytes out of the endpoint and %d into it through the client; want none", s-served, r-received)
			}
			if copies, _ := f.srv.Copies(); copies != 1 {
				t.Errorf("%d CopyObject calls, want 1", copies)
			}
			f.wantCopy(t, "dst")
		})
	}
}

// TestCopyPastOneCopyCopiesParts: an object larger than one CopyObject copies (S3's 5 GiB, scaled down here to
// 1 MiB, which the endpoint enforces as S3 does) is copied part by part with UploadPartCopy, in the source's own
// parts, so the copy's composite checksum is the source's and proves the copy with no byte through the client.
func TestCopyPastOneCopyCopiesParts(t *testing.T) {
	const limit = 1 << 20
	f := newCopyFixture(t, 2*limit+limit/2+3, 256<<10)
	f.srv.SetMaxPut(limit)
	f.s.MaxCopySize = limit
	served, received := f.counts()
	got, err := f.s.Copy(t.Context(), "src", "dst", f.src.Digest)
	if want := (snapstore.Object{Key: "dst", Digest: f.src.Digest, Size: f.src.Size}); err != nil || got != want {
		t.Fatalf("Copy = %+v, %v; want %+v", got, err, want)
	}
	s, r := f.counts()
	if s != served || r != received {
		t.Errorf("the copy moved %d bytes out of the endpoint and %d into it through the client; want none", s-served, r-received)
	}
	if copies, parts := f.srv.Copies(); copies != 0 || parts != 11 {
		t.Errorf("%d CopyObject and %d UploadPartCopy calls; want 0 and the source's 11 parts", copies, parts)
	}
	if n := f.srv.Uploads(); n != 0 {
		t.Errorf("%d multipart uploads left open", n)
	}
	f.wantCopy(t, "dst")
}

// TestCopyRefusesACorruptCopy: a copy the endpoint stored other bytes for is refused (ErrDigestMismatch) and leaves
// nothing under its key; from an endpoint that keeps checksums, by the checksum alone, without reading the copy.
func TestCopyRefusesACorruptCopy(t *testing.T) {
	for _, c := range []struct {
		name     string
		maxCopy  int64
		checksum bool
	}{{"CopyObject", 0, true}, {"UploadPartCopy", 512 << 10, true}, {"CopyObject, no checksums", 0, false}, {"UploadPartCopy, no checksums", 512 << 10, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t, 1<<20+17, 256<<10)
			f.s.MaxCopySize = c.maxCopy
			f.srv.SetNoChecksum(!c.checksum)
			served, _ := f.counts()
			f.srv.SetFault(s3test.Corrupt)
			got, err := f.s.Copy(t.Context(), "src", "dst", f.src.Digest)
			f.srv.SetFault(s3test.None)
			if !errors.Is(err, snapstore.ErrDigestMismatch) {
				t.Fatalf("Copy that the endpoint stored corrupt = %+v, %v; want ErrDigestMismatch", got, err)
			}
			if o, err := f.s.Stat(t.Context(), "dst"); !errors.Is(err, snapstore.ErrNotFound) {
				t.Errorf("Stat after a refused copy = %+v, %v; want ErrNotFound", o, err)
			}
			if s, _ := f.counts(); c.checksum && s != served {
				t.Errorf("refusing the copy read %d bytes; the endpoint's checksum proves it without them", s-served)
			}
			if n := f.srv.Uploads(); n != 0 {
				t.Errorf("%d multipart uploads left open", n)
			}
			f.wantCopy(t, "src")
		})
	}
}

// TestCopyWithoutChecksumsReadsTheCopy: from an endpoint that keeps no checksums the copy is still server-side, and
// it is proved by reading it once: nothing goes up, and what comes down is the copy's bytes once.
func TestCopyWithoutChecksumsReadsTheCopy(t *testing.T) {
	f := newCopyFixture(t, 1<<20, 0)
	f.srv.SetNoChecksum(true)
	served, received := f.counts()
	if _, err := f.s.Copy(t.Context(), "src", "dst", f.src.Digest); err != nil {
		t.Fatal(err)
	}
	if s, r := f.counts(); s-served != int64(len(f.data)) || r != received {
		t.Errorf("the copy moved %d bytes out and %d in; want the copy read once (%d) and none in", s-served, r-received, len(f.data))
	}
	f.wantCopy(t, "dst")
}

// TestCopyFallsBackToStreaming: an endpoint that does not implement a server-side copy (501 NotImplemented) has the
// object streamed through the client instead, in one call and in parts; one that fails the copy otherwise (503) fails
// it, and nothing is streamed.
func TestCopyFallsBackToStreaming(t *testing.T) {
	for _, c := range []struct {
		name    string
		maxCopy int64
	}{{"CopyObject", 0}, {"UploadPartCopy", 512 << 10}} {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t, 1<<20+17, 256<<10)
			f.s.MaxCopySize = c.maxCopy
			f.srv.SetNoCopy(true)
			_, received := f.counts()
			got, err := f.s.Copy(t.Context(), "src", "dst", f.src.Digest)
			if want := (snapstore.Object{Key: "dst", Digest: f.src.Digest, Size: f.src.Size}); err != nil || got != want {
				t.Fatalf("Copy from an endpoint that copies nothing = %+v, %v; want %+v", got, err, want)
			}
			if _, r := f.counts(); r-received != int64(len(f.data)) {
				t.Errorf("%d bytes went up; want the object streamed (%d)", r-received, len(f.data))
			}
			if n := f.srv.Uploads(); n != 0 {
				t.Errorf("%d multipart uploads left open", n)
			}
			f.wantCopy(t, "dst")

			f.srv.SetNoCopy(false)
			_, received = f.counts()
			f.srv.SetFault(s3test.Refuse)
			got, err = f.s.Copy(t.Context(), "src", "dst2", f.src.Digest)
			f.srv.SetFault(s3test.None)
			if err == nil {
				t.Fatalf("Copy the endpoint answered 503 = %+v, nil", got)
			}
			if _, r := f.counts(); r != received {
				t.Errorf("a copy that failed with 503 streamed %d bytes; a transient failure is not a fallback", r-received)
			}
			if o, err := f.s.Stat(t.Context(), "dst2"); !errors.Is(err, snapstore.ErrNotFound) {
				t.Errorf("Stat after a failed copy = %+v, %v; want ErrNotFound", o, err)
			}
		})
	}
}
