package s3

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/storetest"
)

// TestRealEndpoint runs the contract against a real S3-compatible endpoint (e.g. MinIO) when HERD_S3_TEST_ENDPOINT
// and HERD_S3_TEST_BUCKET name one and AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY its credentials (HERD_S3_TEST_REGION
// optional). Every run writes under its own prefix and deletes what it wrote. Faults cannot be injected there; Tamper
// overwrites an object's bytes with a raw PUT that keeps its recorded digest.
func TestRealEndpoint(t *testing.T) {
	ep, bucket := os.Getenv("HERD_S3_TEST_ENDPOINT"), os.Getenv("HERD_S3_TEST_BUCKET")
	if ep == "" || bucket == "" {
		t.Skip("HERD_S3_TEST_ENDPOINT and HERD_S3_TEST_BUCKET name no endpoint")
	}
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Endpoint: ep, Region: os.Getenv("HERD_S3_TEST_REGION"), Bucket: bucket, Prefix: "herd-test-" + hex.EncodeToString(id[:]),
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
	open := func(t *testing.T) *Store {
		s, err := New(cfg, &http.Client{Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	var written []string
	t.Cleanup(func() {
		s := open(t)
		for _, k := range written {
			_ = s.Delete(t.Context(), k)
		}
	})
	written = append(written, "s1/a1/1", "big", "rest", "empty", "k", "d/1", "f/cancel", "f/broken", "t/1")
	storetest.Run(t, storetest.Harness{
		Open: func(t *testing.T) snapstore.Store { return open(t) },
		Tamper: func(t *testing.T, key string) {
			s := open(t)
			o, err := s.Stat(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("y", int(o.Size))
			hdr := http.Header{}
			hdr.Set(DigestHeader, o.Digest)
			sum := digestHex([]byte(body))
			resp, err := s.do(t.Context(), http.MethodPut, key, hdr, strings.NewReader(body), o.Size, sum)
			if err != nil {
				t.Fatal(err)
			}
			drain(resp)
			if resp.StatusCode/100 != 2 {
				t.Fatalf("tamper %s: HTTP %d", key, resp.StatusCode)
			}
		},
	})
}
