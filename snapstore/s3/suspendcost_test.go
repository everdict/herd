package s3_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// summedSource is a bundle as herdlet's writer leaves it (PLT-591): a file it can read at offsets, whose digest and
// part sums the write already made (Sums). It counts every byte read from it.
type summedSource struct {
	data []byte
	off  int64
	read atomic.Int64
	// sums, if set, replaces what Sums answers.
	sums func(part int64) (int64, []byte, [][]byte, bool)
}

func (s *summedSource) Read(p []byte) (int, error) {
	if s.off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.off:])
	s.off += int64(n)
	s.read.Add(int64(n))
	return n, nil
}

func (s *summedSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	s.read.Add(int64(n))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *summedSource) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += s.off
	case io.SeekEnd:
		off += int64(len(s.data))
	}
	s.off = off
	return off, nil
}

// Sums answers the size, the SHA-256 of the whole source and that of each consecutive part bytes of it.
func (s *summedSource) Sums(part int64) (int64, []byte, [][]byte, bool) {
	if s.sums != nil {
		return s.sums(part)
	}
	whole := sha256.Sum256(s.data)
	var parts [][]byte
	for off := int64(0); off < int64(len(s.data)); off += part {
		p := sha256.Sum256(s.data[off:min(off+part, int64(len(s.data)))])
		parts = append(parts, p[:])
	}
	return int64(len(s.data)), whole[:], parts, true
}

func newStore(t *testing.T, srv *s3test.Server, rt http.RoundTripper, part int64) *s3.Store {
	t.Helper()
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey},
		&http.Client{Transport: rt, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.PartSize = part
	return s
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPutReadsItsSourceOnce (PLT-591): a Put of a bundle whose digest and part sums its write already made reads the
// bundle once, to upload it, in one PUT and in parts; the object holds its bytes.
func TestPutReadsItsSourceOnce(t *testing.T) {
	data := bytes.Repeat([]byte("a page of guest memory "), (8<<20)/23)
	for _, c := range []struct {
		name string
		part int64
	}{{"one PUT", 64 << 20}, {"parts", 1 << 20}} {
		t.Run(c.name, func(t *testing.T) {
			srv := s3test.New("us-east-1", creds)
			srv.SetMinPartSize(0)
			s := newStore(t, srv, http.DefaultTransport, c.part)
			src := &summedSource{data: data}
			o, err := s.Put(t.Context(), "bundle", src)
			if err != nil || o.Digest != digestOf(data) || o.Size != int64(len(data)) {
				t.Fatalf("Put = %+v, %v", o, err)
			}
			if n := src.read.Load(); n != int64(len(data)) {
				t.Errorf("Put read %d bytes of a %d-byte source, want each byte once", n, len(data))
			}
			if _, err := s.Verify(t.Context(), "bundle", o.Digest); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
}

// TestPutRefusesSumsOfOtherBytes (PLT-591): a source whose bytes are not those its sums were made of (it changed
// after its write) is refused, and no object is left.
func TestPutRefusesSumsOfOtherBytes(t *testing.T) {
	data := bytes.Repeat([]byte("memory "), (3<<20)/7)
	other := bytes.Clone(data)
	other[len(other)/2] ^= 1
	for _, c := range []struct {
		name string
		part int64
	}{{"one PUT", 64 << 20}, {"parts", 1 << 20}} {
		t.Run(c.name, func(t *testing.T) {
			srv := s3test.New("us-east-1", creds)
			srv.SetMinPartSize(0)
			s := newStore(t, srv, http.DefaultTransport, c.part)
			sums := (&summedSource{data: other}).Sums
			o, err := s.Put(t.Context(), "bundle", &summedSource{data: data, sums: sums})
			if err == nil {
				t.Fatalf("Put of bytes other than its sums' = %+v, nil", o)
			}
			if o, err := s.Stat(t.Context(), "bundle"); !errors.Is(err, snapstore.ErrNotFound) {
				t.Errorf("Stat after the refused Put = %+v, %v; want ErrNotFound", o, err)
			}
			if n := srv.Uploads(); n != 0 {
				t.Errorf("%d multipart uploads left open", n)
			}
		})
	}
}

// holdParts is a transport that holds every part upload until hold parts are in flight together (or a second
// passed), and records the most it saw in flight; fail, if set, refuses the part numbered so.
type holdParts struct {
	hold int
	fail string
	mu   sync.Mutex
	cond *sync.Cond
	in   int
	most int
}

func newHoldParts(hold int, fail string) *holdParts {
	h := &holdParts{hold: hold, fail: fail}
	h.cond = sync.NewCond(&h.mu)
	return h
}

func (h *holdParts) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	if r.Method != http.MethodPut || q.Get("partNumber") == "" {
		return http.DefaultTransport.RoundTrip(r)
	}
	h.mu.Lock()
	h.in++
	h.most = max(h.most, h.in)
	h.cond.Broadcast()
	deadline := time.Now().Add(time.Second)
	for h.in < h.hold && time.Now().Before(deadline) {
		go func() { time.Sleep(10 * time.Millisecond); h.cond.Broadcast() }()
		h.cond.Wait()
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.in--
		h.mu.Unlock()
	}()
	if q.Get("partNumber") == h.fail {
		if r.Body != nil {
			r.Body.Close()
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("<Error><Code>SlowDown</Code></Error>")),
			Header: http.Header{}, Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h *holdParts) mostInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.most
}

// TestPartsGoUpTogether (PLT-591): a multipart Put sends its parts four at once by default, never more; the object
// is whole and verified.
func TestPartsGoUpTogether(t *testing.T) {
	data := bytes.Repeat([]byte("guest memory "), (10<<20)/13)
	srv := s3test.New("us-east-1", creds)
	srv.SetMinPartSize(0)
	rt := newHoldParts(4, "")
	s := newStore(t, srv, rt, 1<<20)
	o, err := s.Put(t.Context(), "bundle", bytes.NewReader(data))
	if err != nil || o.Digest != digestOf(data) {
		t.Fatalf("Put = %+v, %v", o, err)
	}
	if n := rt.mostInFlight(); n != 4 {
		t.Errorf("at most %d parts were in flight at once, want 4 (the default)", n)
	}
	if _, err := s.Verify(t.Context(), "bundle", o.Digest); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestFailedPartFailsThePut (PLT-591): with parts in flight together, one refused part fails the whole Put, which
// aborts the upload and leaves no object: an unreadable or failed part is never a stored object.
func TestFailedPartFailsThePut(t *testing.T) {
	data := bytes.Repeat([]byte("guest memory "), (10<<20)/13)
	srv := s3test.New("us-east-1", creds)
	srv.SetMinPartSize(0)
	s := newStore(t, srv, newHoldParts(4, "3"), 1<<20)
	if o, err := s.Put(t.Context(), "bundle", bytes.NewReader(data)); err == nil {
		t.Fatalf("Put with part 3 refused = %+v, nil", o)
	}
	if n := srv.Uploads(); n != 0 {
		t.Errorf("%d multipart uploads left open after a failed Put", n)
	}
	if o, err := s.Stat(t.Context(), "bundle"); !errors.Is(err, snapstore.ErrNotFound) {
		t.Errorf("Stat after a failed Put = %+v, %v; want ErrNotFound", o, err)
	}
}

// TestConcurrencyBoundsThePartsInFlight (PLT-591): Store.Concurrency is the most parts in flight at once.
func TestConcurrencyBoundsThePartsInFlight(t *testing.T) {
	data := bytes.Repeat([]byte("guest memory "), (3<<20)/13)
	for _, c := range []int{1, 2} {
		srv := s3test.New("us-east-1", creds)
		srv.SetMinPartSize(0)
		rt := newHoldParts(c+1, "")
		s := newStore(t, srv, rt, 1<<20)
		s.Concurrency = c
		o, err := s.Put(t.Context(), "bundle", bytes.NewReader(data))
		if err != nil || o.Digest != digestOf(data) {
			t.Fatalf("Concurrency %d: Put = %+v, %v", c, o, err)
		}
		if n := rt.mostInFlight(); n != c {
			t.Errorf("Concurrency %d: at most %d parts were in flight at once", c, n)
		}
	}
}
