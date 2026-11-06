package s3_test

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// TestMeasureSnapshotStore (DEFAUL-1601) measures, on a synthetic memory image and an in-process S3 endpoint over
// loopback, the bytes a snapshot object stores and the seconds per GiB its Put and a restore's read take, and what
// skipping or compressing its zero pages would store and cost. It runs only when HERD_MEASURE is set
// (HERD_MEASURE_MIB, default 512, is the image's size; HERD_MEASURE_ZERO, default 0.85, the share of its 4 KiB pages
// that are zero). Each line it logs starts with MEASURE.
func TestMeasureSnapshotStore(t *testing.T) {
	if os.Getenv("HERD_MEASURE") == "" {
		t.Skip("HERD_MEASURE is not set")
	}
	mib, zero := 512, 0.85
	if v, err := strconv.Atoi(os.Getenv("HERD_MEASURE_MIB")); err == nil {
		mib = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("HERD_MEASURE_ZERO"), 64); err == nil {
		zero = v
	}
	const page = 4096
	size := int64(mib) << 20
	gib := float64(size) / (1 << 30)
	path := filepath.Join(t.TempDir(), "memory")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Pages are zero or random in runs, as a guest's touched and untouched memory is: a run of 256 pages (1 MiB) is
	// all zero with probability zero.
	var coin [1]byte
	run := make([]byte, 256*page)
	for off := int64(0); off < size; off += int64(len(run)) {
		if _, err := rand.Read(coin[:]); err != nil {
			t.Fatal(err)
		}
		clear(run)
		if float64(coin[0])/256 >= zero {
			if _, err := rand.Read(run); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.Write(run); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	logf := func(format string, a ...any) { t.Logf("MEASURE "+format, a...) }
	logf("image_bytes=%d zero_share_asked=%.2f", size, zero)

	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		src, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("m%d", i)
		start := time.Now()
		o, err := s.Put(t.Context(), key, src)
		put := time.Since(start)
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		rc, err := s.Open(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		rc.Close()
		get := time.Since(start)
		if err != nil || n != o.Size {
			t.Fatalf("read back %d bytes: %v", n, err)
		}
		logf("run=%d stored_bytes=%d put_s_per_gib=%.3f read_s_per_gib=%.3f", i, srv.Size("/herd/"+key), put.Seconds()/gib, get.Seconds()/gib)
		if err := s.Delete(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}

	// Zero pages: what a bundle that left them out would store, and what finding them costs.
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zeroPage := make([]byte, page)
	start := time.Now()
	nonzero := 0
	for off := 0; off < len(img); off += page {
		if !bytes.Equal(img[off:off+page], zeroPage) {
			nonzero++
		}
	}
	scan := time.Since(start)
	logf("zero_pages=%d of %d skipped_bytes_stored=%d scan_s_per_gib=%.3f", len(img)/page-nonzero, len(img)/page, int64(nonzero)*page+int64(len(img)/page/8), scan.Seconds()/gib)

	// Compression of the whole image at flate's fastest level.
	var cw countWriter
	start = time.Now()
	fw, err := flate.NewWriter(&cw, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(img); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	logf("flate_bytes=%d flate_s_per_gib=%.3f", cw.n, time.Since(start).Seconds()/gib)
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
