package s3_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/snapstore/storetest"
)

var creds = s3.Credentials{AccessKeyID: "herd-test", SecretAccessKey: "herd-test-secret"}

func fakeHarness(t *testing.T, noChecksum bool) storetest.Harness {
	srv := s3test.New("us-east-1", creds)
	srv.SetNoChecksum(noChecksum)
	ts := s3test.Start(t, srv)
	cfg := s3.Config{Endpoint: ts.URL, Bucket: "herd", Prefix: "snaps", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}
	return storetest.Harness{
		Open: func(t *testing.T) snapstore.Store {
			s, err := s3.New(cfg, &http.Client{Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
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

// TestContract: the S3 store keeps the snapshot store contract against an in-process S3 endpoint, both one that
// reports its own checksum of what it stored and one that keeps none (Put then reads the object back).
func TestContract(t *testing.T) {
	t.Run("checksums", func(t *testing.T) { storetest.Run(t, fakeHarness(t, false)) })
	t.Run("no checksums", func(t *testing.T) { storetest.Run(t, fakeHarness(t, true)) })
}

// TestWrongCredentialsFail: an endpoint that refuses the signature fails every call; nothing answers success.
func TestWrongCredentialsFail(t *testing.T) {
	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: "wrong"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o, err := s.Put(t.Context(), "k", http.NoBody); err == nil {
		t.Errorf("Put with a wrong secret = %+v, nil", o)
	}
	if _, err := s.Stat(t.Context(), "k"); err == nil {
		t.Error("Stat with a wrong secret answered")
	}
	if srv.Puts() != 0 {
		t.Errorf("the endpoint stored %d objects", srv.Puts())
	}
}

func TestConfig(t *testing.T) {
	c, err := s3.ParseURL("s3://herd-snapshots/pool-1/")
	if err != nil || c.Bucket != "herd-snapshots" || c.Prefix != "pool-1" {
		t.Errorf("ParseURL = %+v, %v", c, err)
	}
	for _, bad := range []string{"herd", "s3://", "http://b/p", "s3://b/p?x=1"} {
		if _, err := s3.ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) accepted", bad)
		}
	}
	ok := s3.Config{Endpoint: "http://minio:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}
	if s, err := s3.New(ok, nil); err != nil || s.Config().Region != "us-east-1" {
		t.Errorf("New(%+v) = %v", ok, err)
	}
	for _, c := range []s3.Config{
		{Endpoint: "minio:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"},
		{Endpoint: "http://minio:9000/path", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"},
		{Endpoint: "http://minio:9000", AccessKeyID: "a", SecretAccessKey: "s"},
		{Endpoint: "http://minio:9000", Bucket: "b"},
		{Endpoint: "http://minio:9000", Bucket: "b", Prefix: "../x", AccessKeyID: "a", SecretAccessKey: "s"},
	} {
		if _, err := s3.New(c, nil); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
}
