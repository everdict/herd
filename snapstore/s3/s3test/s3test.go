// Package s3test is an in-process S3 endpoint for tests: path-style PUT, GET, HEAD and DELETE of objects and the
// multipart upload calls (create, upload part, complete, abort), SigV4 checked against one set of credentials, the
// signed payload hash and x-amz-checksum-sha256 checked against the body as S3 does, S3's composite checksum of a
// multipart object, and switches for the faults a real endpoint or the network between can produce.
package s3test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
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

// Fault is what the endpoint does to the next PUTs; for a multipart upload, Refuse applies to its parts and
// DropAnswer and Corrupt to its completion.
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

// MinPartSize is the smallest part S3 takes, but for the last of an upload.
const MinPartSize = 5 << 20

type object struct {
	data   []byte
	digest string
	// composite is S3's checksum of an object a multipart upload made: "<base64 SHA-256 of the parts' SHA-256s>-<n>",
	// of the parts as the endpoint stored them. Empty for one PUT, whose checksum is its bytes' SHA-256.
	composite string
}

// checksum is what HEAD reports as the object's x-amz-checksum-sha256.
func (o object) checksum() string {
	if o.composite != "" {
		return o.composite
	}
	sum := sha256.Sum256(o.data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

type part struct {
	data []byte
	sum  [32]byte
}

type upload struct {
	path   string
	digest string
	parts  map[int]part
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
	// maxPut, if positive, is the largest body one PUT takes (S3's is 5 GiB).
	maxPut  int64
	minPart int64
	uploads map[string]*upload
	nextID  int
	parts   int
}

// New returns a server that accepts creds in region.
func New(region string, creds s3.Credentials) *Server {
	return &Server{Region: region, Creds: creds, objects: map[string]object{}, minPart: MinPartSize, uploads: map[string]*upload{}}
}

// SetMaxPut makes a PUT of more than n bytes, an object's or a part's, answer EntityTooLarge, as S3 does past 5 GiB;
// 0 lifts the limit.
func (s *Server) SetMaxPut(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxPut = n
}

// SetMinPartSize sets the smallest part a multipart upload may complete with, but for its last (default
// MinPartSize).
func (s *Server) SetMinPartSize(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minPart = n
}

// Uploads counts the multipart uploads created and neither completed nor aborted: parts an endpoint keeps (and
// bills) for an upload nobody finished.
func (s *Server) Uploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// Parts counts the parts the server took, over all uploads.
func (s *Server) Parts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parts
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
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.createUpload(w, r, path)
		return
	case r.Method == http.MethodPut && q.Has("uploadId"):
		s.uploadPart(w, r, path, q.Get("uploadId"), q.Get("partNumber"))
		return
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.completeUpload(w, r, path, q.Get("uploadId"))
		return
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.mu.Lock()
		u, ok := s.uploads[q.Get("uploadId")]
		if ok && u.path == path {
			delete(s.uploads, q.Get("uploadId"))
		}
		s.mu.Unlock()
		if !ok || u.path != path {
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
			w.Header().Set(s3.ChecksumHeader, o.checksum())
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

// body reads a PUT's body and checks it against its signed payload hash and checksum, as S3 does.
func (s *Server) body(w http.ResponseWriter, r *http.Request) ([]byte, [32]byte, bool) {
	if r.ContentLength < 0 {
		s3Error(w, http.StatusLengthRequired, "MissingContentLength")
		return nil, [32]byte{}, false
	}
	s.mu.Lock()
	maxPut := s.maxPut
	s.mu.Unlock()
	if maxPut > 0 && r.ContentLength > maxPut {
		s3Error(w, http.StatusBadRequest, "EntityTooLarge")
		return nil, [32]byte{}, false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || int64(len(data)) != r.ContentLength {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return nil, [32]byte{}, false
	}
	sum := sha256.Sum256(data)
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		return nil, [32]byte{}, false
	}
	if c := r.Header.Get(s3.ChecksumHeader); c != "" && c != base64.StdEncoding.EncodeToString(sum[:]) {
		s3Error(w, http.StatusBadRequest, "BadDigest")
		return nil, [32]byte{}, false
	}
	return data, sum, true
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, path string) {
	data, _, ok := s.body(w, r)
	if !ok {
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
		dropAnswer(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// dropAnswer cuts the connection without answering.
func dropAnswer(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	// No connection to cut: an error answer for bytes it stored leaves the client as unsure.
	s3Error(w, http.StatusInternalServerError, "InternalError")
}

func etag(sum [32]byte) string { return `"` + hex.EncodeToString(sum[:16]) + `"` }

// createUpload is CreateMultipartUpload. The fake keeps SHA-256 checksums only, so an upload must ask for them.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("X-Amz-Checksum-Algorithm") != "SHA256" {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	s.mu.Lock()
	s.nextID++
	id := "upload-" + strconv.Itoa(s.nextID)
	s.uploads[id] = &upload{path: path, digest: r.Header.Get(s3.DigestHeader), parts: map[int]part{}}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, path, id, number string) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > 10000 {
		s3Error(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	data, sum, ok := s.body(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok || u.path != path {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	if s.fault == Refuse {
		s3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	u.parts[n] = part{data: data, sum: sum}
	s.parts++
	w.Header().Set("ETag", etag(sum))
	w.WriteHeader(http.StatusOK)
}

type completeRequest struct {
	Parts []struct {
		PartNumber     int
		ETag           string
		ChecksumSHA256 string
	} `xml:"Part"`
}

// completeUpload is CompleteMultipartUpload: the parts, named in ascending order with the ETag and checksum each was
// answered with, become the object.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, path, id string) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	if sum := sha256.Sum256(b); r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		return
	}
	var req completeRequest
	if err := xml.Unmarshal(b, &req); err != nil || len(req.Parts) == 0 {
		s3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	s.mu.Lock()
	u, ok := s.uploads[id]
	if !ok || u.path != path {
		s.mu.Unlock()
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var data, sums []byte
	for i, p := range req.Parts {
		got, ok := u.parts[p.PartNumber]
		if i > 0 && p.PartNumber <= req.Parts[i-1].PartNumber {
			s.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "InvalidPartOrder")
			return
		}
		if !ok || p.ETag != etag(got.sum) || p.ChecksumSHA256 != base64.StdEncoding.EncodeToString(got.sum[:]) {
			s.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		if i < len(req.Parts)-1 && int64(len(got.data)) < s.minPart {
			s.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "EntityTooSmall")
			return
		}
		d, sum := got.data, got.sum
		if s.fault == Corrupt && i == len(req.Parts)-1 && len(d) > 0 {
			// The endpoint's checksum is of what it holds, and the last part as it stored it is not what it was sent.
			d = bytes.Clone(d)
			d[len(d)-1] ^= 0x01
			sum = sha256.Sum256(d)
		}
		data = append(data, d...)
		sums = append(sums, sum[:]...)
	}
	fault := s.fault
	composite := sha256.Sum256(sums)
	s.objects[path] = object{data: data, digest: u.digest,
		composite: base64.StdEncoding.EncodeToString(composite[:]) + "-" + strconv.Itoa(len(req.Parts))}
	delete(s.uploads, id)
	s.puts++
	s.mu.Unlock()
	if fault == DropAnswer {
		dropAnswer(w)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>", path)
}
