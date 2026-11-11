// Package s3test is an in-process S3 endpoint for tests: path-style PUT, GET, HEAD and DELETE of objects, the
// multipart upload calls (create, upload part, complete, abort), the server-side copies (CopyObject, UploadPartCopy)
// and HEAD of one part (?partNumber), SigV4 checked against one set of credentials, the signed payload hash and
// x-amz-checksum-sha256 checked against the body as S3 does, S3's composite checksum of a multipart object, and
// switches for the faults a real endpoint or the network between can produce. It lists a bucket's objects
// (ListObjectsV2) and its unfinished multipart uploads (ListMultipartUploads), a page at a time.
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everdict/herd/snapstore/s3"
)

// Fault is what the endpoint does to the next PUTs; for a multipart upload, Refuse applies to its parts and
// DropAnswer and Corrupt to its completion; a CopyObject is a PUT, and an UploadPartCopy a part.
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
	// parts are the sizes of the parts of an object a multipart upload made, nil for one PUT.
	parts []int64
	// recorded is the object's s3.ChecksumMetaHeader metadata.
	recorded string
	// modified is when it was stored (LastModified).
	modified time.Time
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
	initiated time.Time
	path      string
	digest    string
	recorded  string
	parts     map[int]part
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
	// writeRate, if positive, is the bytes per second a PUT's body is taken at.
	writeRate int64
	served    int64
	received  int64
	// maxPut, if positive, is the largest body one PUT takes (S3's is 5 GiB).
	maxPut  int64
	minPart int64
	uploads map[string]*upload
	nextID  int
	parts   int
	// noCopy makes CopyObject and UploadPartCopy answer 501 NotImplemented (an endpoint that copies nothing).
	noCopy     bool
	copies     int
	partCopies int
	// now, if set, is the server's clock (LastModified, Initiated); listPage is the most keys or uploads one list
	// answer carries (default 1000, S3's); lists counts the list calls.
	now      func() time.Time
	listPage int
	lists    int
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetClock makes now the server's clock for the times it records: an object's LastModified and an upload's Initiated.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// SetListPage sets the most keys (or uploads) one list answer carries; the rest follow a continuation, as S3 pages
// past 1000.
func (s *Server) SetListPage(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listPage = n
}

// Lists counts the list calls the server answered (objects and uploads, every page).
func (s *Server) Lists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// Begin starts a multipart upload of path ("/bucket/key") that nobody finishes, as a writer that died midway leaves
// one, and answers its id.
func (s *Server) Begin(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := "upload-" + strconv.Itoa(s.nextID)
	s.uploads[id] = &upload{initiated: s.clock(), path: path, parts: map[int]part{}}
	return id
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

// SetWriteRate takes every PUT's body at bytesPerSecond from now on (0: as fast as the connection goes), as an
// endpoint across a slow link does.
func (s *Server) SetWriteRate(bytesPerSecond int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeRate = bytesPerSecond
}

// slowReader reads at most what a link of rate bytes per second carried since start.
type slowReader struct {
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (s *slowReader) Read(p []byte) (int, error) {
	for {
		if allowed := s.rate*int64(time.Since(s.start))/int64(time.Second) - s.n; allowed > 0 {
			n, err := s.r.Read(p[:min(int64(len(p)), allowed)])
			s.n += int64(n)
			return n, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SetNoCopy makes the server refuse every server-side copy with 501 NotImplemented, as an endpoint that does not
// implement CopyObject and UploadPartCopy answers.
func (s *Server) SetNoCopy(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noCopy = on
}

// Copies counts the CopyObject calls the server stored; partCopies the UploadPartCopy calls it took.
func (s *Server) Copies() (copies, partCopies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copies, s.partCopies
}

// Received counts the bytes the server took in PUT bodies (objects and parts).
func (s *Server) Received() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
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

// Plant stores data at path ("/bucket/key") as an object whose herd digest metadata is digest and whose recorded
// checksum metadata (s3.ChecksumMetaHeader) is recorded, whatever the bytes are: what a writer that lied, or an
// endpoint that broke after it took the bytes, would leave. With partSize positive the object is made of parts of
// that size, and its checksum is their composite one.
func (s *Server) Plant(path string, data []byte, digest, recorded string, partSize int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := object{data: bytes.Clone(data), digest: digest, recorded: recorded, modified: s.clock()}
	if partSize > 0 {
		var sums []byte
		for off := 0; off < len(data); off += partSize {
			p := data[off:min(off+partSize, len(data))]
			sum := sha256.Sum256(p)
			sums = append(sums, sum[:]...)
			o.parts = append(o.parts, int64(len(p)))
		}
		composite := sha256.Sum256(sums)
		o.composite = base64.StdEncoding.EncodeToString(composite[:]) + "-" + strconv.Itoa(len(o.parts))
	}
	s.objects[path] = o
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
	q := r.URL.Query()
	if bucket, ok := strings.CutPrefix(strings.TrimSuffix(path, "/"), "/"); ok && bucket != "" && !strings.Contains(bucket, "/") && r.Method == http.MethodGet {
		switch {
		case q.Get("list-type") == "2":
			s.listObjects(w, bucket, q)
			return
		case q.Has("uploads"):
			s.listUploads(w, bucket, q)
			return
		}
	}
	if strings.Count(path, "/") < 2 {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.createUpload(w, r, path)
		return
	case r.Method == http.MethodPut && q.Has("uploadId") && r.Header.Get("X-Amz-Copy-Source") != "":
		s.uploadPartCopy(w, r, path, q.Get("uploadId"), q.Get("partNumber"))
		return
	case r.Method == http.MethodPut && q.Has("uploadId"):
		s.uploadPart(w, r, path, q.Get("uploadId"), q.Get("partNumber"))
		return
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		s.copyObject(w, r, path)
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
		size := int64(len(o.data))
		if pn := q.Get("partNumber"); pn != "" && r.Method == http.MethodHead {
			// One part of a multipart object; an object one PUT made is its own part 1.
			n, err := strconv.Atoi(pn)
			if err != nil || n < 1 || (o.parts == nil && n != 1) || (o.parts != nil && n > len(o.parts)) {
				s3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber")
				return
			}
			if o.parts != nil {
				size = o.parts[n-1]
				w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(o.parts)))
			}
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set(s3.DigestHeader, o.digest)
		if o.recorded != "" {
			w.Header().Set(s3.ChecksumMetaHeader, o.recorded)
		}
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
	maxPut, rate := s.maxPut, s.writeRate
	s.mu.Unlock()
	if maxPut > 0 && r.ContentLength > maxPut {
		s3Error(w, http.StatusBadRequest, "EntityTooLarge")
		return nil, [32]byte{}, false
	}
	var body io.Reader = r.Body
	if rate > 0 {
		body = &slowReader{r: r.Body, rate: rate, start: time.Now()}
	}
	data, err := io.ReadAll(body)
	if err != nil || int64(len(data)) != r.ContentLength {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return nil, [32]byte{}, false
	}
	s.mu.Lock()
	s.received += int64(len(data))
	s.mu.Unlock()
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
	s.objects[path] = object{data: stored, digest: r.Header.Get(s3.DigestHeader), modified: s.clock()}
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
	s.uploads[id] = &upload{initiated: s.clock(), path: path, digest: r.Header.Get(s3.DigestHeader), recorded: r.Header.Get(s3.ChecksumMetaHeader), parts: map[int]part{}}
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
	var sizes []int64
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
		sizes = append(sizes, int64(len(d)))
	}
	fault := s.fault
	composite := sha256.Sum256(sums)
	s.objects[path] = object{data: data, digest: u.digest, recorded: u.recorded,
		composite: base64.StdEncoding.EncodeToString(composite[:]) + "-" + strconv.Itoa(len(req.Parts)), parts: sizes, modified: s.clock()}
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

// copySource reads a copy's x-amz-copy-source ("/bucket/key" or "bucket/key", URL-encoded) and the object it names.
func (s *Server) copySource(w http.ResponseWriter, r *http.Request) (object, bool) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		s3Error(w, http.StatusBadRequest, "InvalidArgument")
		return object{}, false
	}
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	s.mu.Lock()
	o, ok := s.objects[src]
	s.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return object{}, false
	}
	return o, true
}

// copyRequest checks what S3 checks of a copy, which has no body: its signed payload hash; and refuses it from an
// endpoint that copies nothing.
func (s *Server) copyRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Amz-Content-Sha256") != s3.EmptySHA256 {
		s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		return false
	}
	s.mu.Lock()
	noCopy := s.noCopy
	s.mu.Unlock()
	if noCopy {
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
		return false
	}
	return true
}

// copyObject is CopyObject: the source's bytes become the object at path, with the source's metadata or, with
// x-amz-metadata-directive: REPLACE, the request's. Its checksum is the SHA-256 of the bytes it stored, as S3 computes
// a full-object checksum for a copy made in one call. Past the largest PUT it answers InvalidRequest, as S3 does past
// 5 GiB.
func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, path string) {
	if !s.copyRequest(w, r) {
		return
	}
	o, ok := s.copySource(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxPut > 0 && int64(len(o.data)) > s.maxPut {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	if s.fault == Refuse {
		s3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	digest, recorded := o.digest, o.recorded
	if r.Header.Get("X-Amz-Metadata-Directive") == "REPLACE" {
		digest, recorded = r.Header.Get(s3.DigestHeader), r.Header.Get(s3.ChecksumMetaHeader)
	}
	data := o.data
	if s.fault == Corrupt && len(data) > 0 {
		data = bytes.Clone(data)
		data[len(data)-1] ^= 0x01
	}
	c := object{data: data, digest: digest, recorded: recorded, modified: s.clock()}
	s.objects[path] = c
	s.copies++
	if s.fault == DropAnswer {
		dropAnswer(w)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	sum := sha256.Sum256(data)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<CopyObjectResult><ETag>%s</ETag><ChecksumSHA256>%s</ChecksumSHA256></CopyObjectResult>",
		etag(sum), base64.StdEncoding.EncodeToString(sum[:]))
}

// uploadPartCopy is UploadPartCopy: the bytes x-amz-copy-source-range names of the source become a part of the
// upload, answered with its ETag and SHA-256 checksum. Past the largest PUT it answers InvalidRequest.
func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, path, id, number string) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > 10000 {
		s3Error(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	if !s.copyRequest(w, r) {
		return
	}
	o, ok := s.copySource(w, r)
	if !ok {
		return
	}
	data := o.data
	if rg := r.Header.Get("X-Amz-Copy-Source-Range"); rg != "" {
		var first, last int64
		if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &first, &last); err != nil || first < 0 || last < first || last >= int64(len(data)) {
			s3Error(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		data = data[first : last+1]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxPut > 0 && int64(len(data)) > s.maxPut {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	u, ok := s.uploads[id]
	if !ok || u.path != path {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	if s.fault == Refuse {
		s3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	sum := sha256.Sum256(data)
	u.parts[n] = part{data: data, sum: sum}
	s.parts++
	s.partCopies++
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<CopyPartResult><ETag>%s</ETag><ChecksumSHA256>%s</ChecksumSHA256></CopyPartResult>",
		etag(sum), base64.StdEncoding.EncodeToString(sum[:]))
}

func (s *Server) page() int {
	if s.listPage > 0 {
		return min(s.listPage, 1000)
	}
	return 1000
}

// listObjects is ListObjectsV2 of the bucket: the keys under prefix in order, a page at a time, the next page named
// by an opaque continuation token (here the base64 of the last key answered, so it carries characters a signer must
// encode).
func (s *Server) listObjects(w http.ResponseWriter, bucket string, q url.Values) {
	after := ""
	if t := q.Get("continuation-token"); t != "" {
		b, err := base64.StdEncoding.DecodeString(t)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		after = string(b)
	}
	s.mu.Lock()
	s.lists++
	root := "/" + bucket + "/"
	var keys []string
	for p := range s.objects {
		if k, ok := strings.CutPrefix(p, root); ok && strings.HasPrefix(k, q.Get("prefix")) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	n := s.page()
	if m, err := strconv.Atoi(q.Get("max-keys")); err == nil && m > 0 {
		n = min(n, m)
	}
	truncated := len(keys) > n
	if truncated {
		keys = keys[:n]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<ListBucketResult xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><IsTruncated>%v</IsTruncated>",
		bucket, q.Get("prefix"), len(keys), truncated)
	for _, k := range keys {
		o := s.objects[root+k]
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>",
			k, o.modified.UTC().Format("2006-01-02T15:04:05.000Z"), len(o.data))
	}
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", base64.StdEncoding.EncodeToString([]byte(keys[len(keys)-1])))
	}
	b.WriteString("</ListBucketResult>")
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

// listUploads is ListMultipartUploads of the bucket: the uploads begun under prefix and neither completed nor aborted,
// ordered by key and then id, a page at a time after key-marker and upload-id-marker.
func (s *Server) listUploads(w http.ResponseWriter, bucket string, q url.Values) {
	type entry struct {
		key, id   string
		initiated time.Time
	}
	s.mu.Lock()
	s.lists++
	root := "/" + bucket + "/"
	var es []entry
	for id, u := range s.uploads {
		k, ok := strings.CutPrefix(u.path, root)
		if !ok || !strings.HasPrefix(k, q.Get("prefix")) {
			continue
		}
		if km := q.Get("key-marker"); km != "" && (k < km || k == km && id <= q.Get("upload-id-marker")) {
			continue
		}
		es = append(es, entry{k, id, u.initiated})
	}
	s.mu.Unlock()
	sort.Slice(es, func(i, j int) bool { return es[i].key < es[j].key || es[i].key == es[j].key && es[i].id < es[j].id })
	s.mu.Lock()
	n := s.page()
	s.mu.Unlock()
	truncated := len(es) > n
	if truncated {
		es = es[:n]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<ListMultipartUploadsResult xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"><Bucket>%s</Bucket><IsTruncated>%v</IsTruncated>", bucket, truncated)
	if truncated {
		last := es[len(es)-1]
		fmt.Fprintf(&b, "<NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>%s</NextUploadIdMarker>", last.key, last.id)
	}
	for _, e := range es {
		fmt.Fprintf(&b, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>", e.key, e.id, e.initiated.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString("</ListMultipartUploadsResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}
