// Package s3test is an in-process S3 endpoint for tests: path-style PUT, GET, HEAD and DELETE of objects, SigV4
// checked against one set of credentials, the signed payload hash and x-amz-checksum-sha256 checked against the body
// as S3 does, and switches for the faults a real endpoint or the network between can produce.
package s3test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everdict/herd/snapstore/s3"
)

// Fault is what the endpoint does to the next PUTs.
type Fault int

const (
	// None stores and answers honestly.
	None Fault = iota
	// Refuse answers 503 and stores nothing.
	Refuse
	// DropAnswer stores the object and then cuts the connection without answering: the outcome is unknown to the
	// client.
	DropAnswer
	// Corrupt answers 200 but stores other bytes than it received (a broken disk or proxy behind the endpoint).
	Corrupt
)

type object struct {
	data   []byte
	digest string
}

// Server is a fake S3 endpoint.
type Server struct {
	Region string
	Creds  s3.Credentials

	mu      sync.Mutex
	objects map[string]object // "/bucket/key"
	fault   Fault
	// noChecksum makes HEAD answer without x-amz-checksum-sha256 (an endpoint that keeps no checksums).
	noChecksum bool
	puts       int
	// readRate, if positive, is the bytes per second a GET's body is sent at (a slow link to the endpoint).
	readRate int64
	served   int64
}

// New returns a server that accepts creds in region.
func New(region string, creds s3.Credentials) *Server {
	return &Server{Region: region, Creds: creds, objects: map[string]object{}}
}

// Start serves s on a loopback httptest server, closed when the test ends.
func Start(t interface{ Cleanup(func()) }, s *Server) *httptest.Server {
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts
}

// SetFault sets what the next PUTs do.
func (s *Server) SetFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = f
}

// SetNoChecksum makes HEAD keep the endpoint's checksum to itself.
func (s *Server) SetNoChecksum(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noChecksum = on
}

// SetReadRate sends every GET's body at bytesPerSecond from now on (0: as fast as the connection goes), as an
// endpoint across a slow link does.
func (s *Server) SetReadRate(bytesPerSecond int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readRate = bytesPerSecond
}

// Served counts the object bytes the server sent in GET bodies.
func (s *Server) Served() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// send writes data as a GET's body, at the read rate if one is set, and counts what it sent.
func (s *Server) send(w http.ResponseWriter, r *http.Request, data []byte) {
	s.mu.Lock()
	rate := s.readRate
	s.mu.Unlock()
	const tick = 20 * time.Millisecond
	start, sent := time.Now(), int64(0)
	for len(data) > 0 {
		n := len(data)
		if rate > 0 {
			// What the link carried by now, at least one byte per tick.
			n = int(min(int64(len(data)), max(1, rate*int64(time.Since(start))/int64(time.Second)-sent)))
		}
		m, err := w.Write(data[:n])
		sent += int64(m)
		s.mu.Lock()
		s.served += int64(m)
		s.mu.Unlock()
		if err != nil {
			return
		}
		data = data[n:]
		if rate > 0 && len(data) > 0 {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-time.After(tick):
			case <-r.Context().Done():
				return
			}
		}
	}
}

// Puts counts the PUTs the server stored.
func (s *Server) Puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// Tamper changes the stored bytes of path ("/bucket/key") behind the client's back; it reports whether they existed.
func (s *Server) Tamper(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[path]
	if !ok || len(o.data) == 0 {
		return ok
	}
	d := append([]byte(nil), o.data...)
	d[0] ^= 0xff
	o.data = d
	s.objects[path] = o
	return true
}

// Size is the length of the object at path ("/bucket/key"), or -1.
func (s *Server) Size(path string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[path]
	if !ok {
		return -1
	}
	return int64(len(o.data))
}

// Keys lists the stored paths.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	return out
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code></Error>", code)
}

// authorized re-signs the request as the client should have and compares.
func (s *Server) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	t, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if got == "" || err != nil {
		return false
	}
	c := r.Clone(r.Context())
	c.Header = r.Header.Clone()
	c.Header.Del("Authorization")
	s3.Sign(c, s.Creds, s.Region, "s3", r.Header.Get("X-Amz-Content-Sha256"), t)
	return c.Header.Get("Authorization") == got
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	path := r.URL.Path
	if strings.Count(path, "/") < 2 {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.put(w, r, path)
	case http.MethodGet, http.MethodHead:
		s.mu.Lock()
		o, ok := s.objects[path]
		noChecksum := s.noChecksum
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.Header().Set(s3.DigestHeader, o.digest)
		if !noChecksum && r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" {
			sum := sha256.Sum256(o.data)
			w.Header().Set(s3.ChecksumHeader, base64.StdEncoding.EncodeToString(sum[:]))
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			s.send(w, r, o.data)
		}
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, path string) {
	if r.ContentLength < 0 {
		s3Error(w, http.StatusLengthRequired, "MissingContentLength")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || int64(len(data)) != r.ContentLength {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	sum := sha256.Sum256(data)
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		return
	}
	if c := r.Header.Get(s3.ChecksumHeader); c != "" && c != base64.StdEncoding.EncodeToString(sum[:]) {
		s3Error(w, http.StatusBadRequest, "BadDigest")
		return
	}
	s.mu.Lock()
	fault := s.fault
	if fault == Refuse {
		s.mu.Unlock()
		s3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	stored := data
	if fault == Corrupt && len(data) > 0 {
		stored = bytes.Clone(data)
		stored[len(stored)-1] ^= 0x01
	}
	s.objects[path] = object{data: stored, digest: r.Header.Get(s3.DigestHeader)}
	s.puts++
	s.mu.Unlock()
	if fault == DropAnswer {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		// No connection to cut: an error answer for bytes it stored leaves the client as unsure.
		s3Error(w, http.StatusInternalServerError, "InternalError")
		return
	}
	w.WriteHeader(http.StatusOK)
}
