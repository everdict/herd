package s3_test

import (
	"crypto/rand"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// TestMeasureCopy (DEFAUL-1610) measures, on a synthetic object and an in-process S3 endpoint over loopback, the
// object bytes that pass through herd and the seconds a tag of a suspended actor (a copy) and a suspend's check take:
// before, a Verify of the source and a stream of it through herd (Open, Put, and Put's own check) and a Verify of the
// suspend's object; after, Copy (one CopyObject, and UploadPartCopy as past 5 GiB) and Prove. It runs only when
// HERD_MEASURE is set (HERD_MEASURE_MIB, default 512, is the object's size, uploaded in 64 MiB parts as a herdlet
// does). Each line it logs starts with MEASURE.
func TestMeasureCopy(t *testing.T) {
	if os.Getenv("HERD_MEASURE") == "" {
		t.Skip("HERD_MEASURE is not set")
	}
	mib := 512
	if v, err := strconv.Atoi(os.Getenv("HERD_MEASURE_MIB")); err == nil {
		mib = v
	}
	size := int64(mib) << 20
	f, err := os.CreateTemp(t.TempDir(), "object")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(f, rand.Reader, size); err != nil {
		t.Fatal(err)
	}
	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	src, err := s.Put(t.Context(), "src", f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	logf := func(format string, a ...any) { t.Logf("MEASURE "+format, a...) }
	logf("object_bytes=%d parts_of=%d", size, s3.DefaultPartSize)
	measure := func(name string, op func() error) {
		t.Helper()
		served, received := srv.Served(), srv.Received()
		start := time.Now()
		if err := op(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		logf("op=%s bytes_through_herd=%d seconds=%.3f", name, srv.Served()-served+srv.Received()-received, time.Since(start).Seconds())
	}
	for i := range 3 {
		key := func(k string) string { return k + strconv.Itoa(i) }
		measure("tag_before_verify_and_stream", func() error {
			if _, err := s.Verify(t.Context(), "src", src.Digest); err != nil {
				return err
			}
			_, err := snapstore.StreamCopy(t.Context(), s, "src", key("stream"), src.Digest)
			return err
		})
		measure("tag_after_copyobject", func() error {
			_, err := s.Copy(t.Context(), "src", key("copy"), src.Digest)
			return err
		})
		s.MaxCopySize = s3.DefaultPartSize // as an object past 5 GiB is copied
		measure("tag_after_uploadpartcopy", func() error {
			_, err := s.Copy(t.Context(), "src", key("parts"), src.Digest)
			return err
		})
		s.MaxCopySize = 0
		measure("suspend_check_before_verify", func() error {
			_, err := s.Verify(t.Context(), "src", src.Digest)
			return err
		})
		measure("suspend_check_after_prove", func() error {
			_, err := s.Prove(t.Context(), "src", src.Digest)
			return err
		})
		for _, k := range []string{"stream", "copy", "parts"} {
			if err := s.Delete(t.Context(), key(k)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if copies, parts := srv.Copies(); copies != 3 || parts != 3*int(size/s3.DefaultPartSize) {
		t.Errorf("%d CopyObject and %d UploadPartCopy calls", copies, parts)
	}
}
