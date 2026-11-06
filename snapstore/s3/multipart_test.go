package s3_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/snapstore/storetest"
)

// TestPutPastOnePut: an object larger than one PUT takes is stored, whole and verified (DEFAUL-1601: a 6.5 GB
// snapshot failed with "bytes is more than one PUT takes"). The endpoint here takes 64 MiB in one PUT, S3's 5 GiB
// scaled down, and the object is larger.
func TestPutPastOnePut(t *testing.T) {
	srv := s3test.New("us-east-1", creds)
	srv.SetMaxPut(64 << 20)
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("memory page "), (65<<20)/12)
	sum := sha256.Sum256(data)
	want := snapstore.Object{Key: "big", Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data))}
	o, err := s.Put(t.Context(), "big", bytes.NewReader(data))
	if err != nil || o != want {
		t.Fatalf("Put of %d bytes = %+v, %v; want %+v", len(data), o, err, want)
	}
	if got, err := s.Verify(t.Context(), "big", want.Digest); err != nil || got != want {
		t.Errorf("Verify = %+v, %v", got, err)
	}
	if srv.Uploads() != 0 {
		t.Errorf("%d multipart uploads left open", srv.Uploads())
	}
}

// multipartHarness is fakeHarness with a store that uploads anything over 4 bytes in parts of 4 bytes, so every
// object of the contract but the smallest goes through a multipart upload.
func multipartHarness(t *testing.T, srv *s3test.Server) storetest.Harness {
	srv.SetMinPartSize(0)
	ts := s3test.Start(t, srv)
	cfg := s3.Config{Endpoint: ts.URL, Bucket: "herd", Prefix: "snaps", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}
	return storetest.Harness{
		Open: func(t *testing.T) snapstore.Store {
			s, err := s3.New(cfg, &http.Client{Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			s.PartSize = 4
			return s
		},
		Tamper: func(t *testing.T, key string) {
			if !srv.Tamper("/herd/snaps/" + key) {
				t.Fatalf("tamper: %s not stored", key)
			}
		},
		Fault: func(t *testing.T, f storetest.WriteFault) {
			srv.SetFault(map[storetest.WriteFault]s3test.Fault{storetest.Honest: s3test.None, storetest.RefuseWrites: s3test.Refuse,
				storetest.LoseAnswers: s3test.DropAnswer, storetest.CorruptWrites: s3test.Corrupt}[f])
		},
	}
}

// TestMultipartContract: through multipart uploads the store keeps the whole snapshot store contract (a failed,
// unknown or corrupted write never answers success and leaves nothing Stat reports), against an endpoint that
// reports its composite checksum of the parts it stored and one that keeps none; and no upload is left open.
func TestMultipartContract(t *testing.T) {
	for _, noChecksum := range []bool{false, true} {
		name := map[bool]string{false: "checksums", true: "no checksums"}[noChecksum]
		t.Run(name, func(t *testing.T) {
			srv := s3test.New("us-east-1", creds)
			srv.SetNoChecksum(noChecksum)
			storetest.Run(t, multipartHarness(t, srv))
			if srv.Parts() == 0 {
				t.Error("no part was uploaded: the contract never ran through a multipart upload")
			}
			if n := srv.Uploads(); n != 0 {
				t.Errorf("%d multipart uploads left open", n)
			}
		})
	}
}

// TestMultipartFailureAborts: a part the endpoint refuses fails the Put, which aborts the upload (S3 keeps the parts
// of an open upload, invisible to Stat, until it is aborted) and leaves no object, not even an older one.
func TestMultipartFailureAborts(t *testing.T) {
	srv := s3test.New("us-east-1", creds)
	h := multipartHarness(t, srv)
	s := h.Open(t)
	if _, err := s.Put(t.Context(), "k", bytes.NewReader([]byte("an older snapshot"))); err != nil {
		t.Fatal(err)
	}
	srv.SetFault(s3test.Refuse)
	o, err := s.Put(t.Context(), "k", bytes.NewReader([]byte("a snapshot in five parts")))
	srv.SetFault(s3test.None)
	if err == nil {
		t.Fatalf("Put with refused parts = %+v, nil", o)
	}
	if n := srv.Uploads(); n != 0 {
		t.Errorf("%d multipart uploads left open after a failed Put", n)
	}
	if o, err := s.Stat(t.Context(), "k"); !errors.Is(err, snapstore.ErrNotFound) {
		t.Errorf("Stat after a failed Put = %+v, %v; want ErrNotFound", o, err)
	}
}
