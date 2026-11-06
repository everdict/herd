// Package s3 is a snapstore.Store in an S3-compatible bucket (AWS S3, MinIO, ...), so herd-api and herdlets on
// different hosts share snapshots without a shared filesystem. It speaks the four object calls it needs (PUT, GET,
// HEAD, DELETE, path-style) signed with SigV4; there is no SDK dependency.
//
// Durability: Put hashes the bytes first and uploads them in one PUT whose signed payload hash
// (x-amz-content-sha256) and x-amz-checksum-sha256 make the endpoint refuse any other bytes; it then HEADs the object
// and answers only when the endpoint reports the size and digest it was given (and, when the endpoint keeps
// checksums, its own SHA-256 of what it stored; without one, Put re-reads the object). An S3 PUT that returned 200 is
// durable. Any failed or unknown outcome deletes the key, so Stat never reports an object after a failed Put — not
// the new bytes, and not the old ones it was replacing.
//
// An object larger than the part size (Store.PartSize) goes up as a multipart upload, since one PUT takes at most
// 5 GiB: each part is hashed in the same first pass and sent with its own signed payload hash and SHA-256 checksum,
// so the endpoint refuses a part that is not those bytes; the completed object's checksum is S3's composite one (the
// SHA-256 of the parts' SHA-256s), which Put checks against the parts it hashed, and the herd digest of the whole
// object is recorded as before. A multipart Put that fails aborts its upload, so the endpoint keeps none of its parts,
// and then deletes the key as any failed Put does.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/everdict/herd/snapstore"
)

// The sizes S3 takes.
const (
	// MaxPutSize is the largest object, or part of one, one PUT takes (5 GiB).
	MaxPutSize = 5 << 30
	// MaxObjectSize is the largest object S3 stores (5 TiB); Put refuses larger ones.
	MaxObjectSize = 5 << 40
	// MaxParts is the most parts one multipart upload has.
	MaxParts = 10000
)

// DefaultPartSize is Store.PartSize's default. An object of up to this size is one PUT.
const DefaultPartSize = 64 << 20

// The object metadata Put records.
const (
	// DigestHeader carries the object's "sha256:<hex>" digest.
	DigestHeader = "X-Amz-Meta-Herd-Digest"
	// ChecksumHeader is S3's full-object SHA-256 checksum (base64), sent on PUT and read back on HEAD.
	ChecksumHeader = "X-Amz-Checksum-Sha256"
	// ChecksumMetaHeader carries, on an object made of parts, the composite checksum the endpoint's own must equal:
	// the one Copy's parts, being the source's, must have.
	ChecksumMetaHeader = "X-Amz-Meta-Herd-Checksum"
)

// Config names a bucket and the credentials to reach it. It is JSON so herd-api can hand it to herdlets in the join
// grant.
type Config struct {
	// Endpoint is the S3 endpoint, scheme://host[:port] (e.g. http://minio.local:9000). Default
	// https://s3.<region>.amazonaws.com.
	Endpoint string `json:"endpoint,omitempty"`
	// Region signs the requests (default us-east-1; MinIO accepts it).
	Region string `json:"region,omitempty"`
	Bucket string `json:"bucket"`
	// Prefix is prepended to every key ("herd/" stores key k at herd/k).
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// ParseURL reads s3://bucket[/prefix] into a Config's bucket and prefix.
func ParseURL(s string) (Config, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.RawQuery != "" || u.User != nil {
		return Config{}, fmt.Errorf("snapstore/s3: %q: want s3://bucket[/prefix]", s)
	}
	return Config{Bucket: u.Host, Prefix: strings.Trim(u.Path, "/")}, nil
}

// Store is a snapstore.Store in one bucket.
type Store struct {
	cfg      Config
	endpoint *url.URL
	prefix   string
	client   *http.Client
	// Now, if set, is the signing clock.
	Now func() time.Time
	// PartSize is the size of the parts Put uploads an object larger than it in (zero: DefaultPartSize). Put raises
	// it for an object that would need more than MaxParts parts. S3 takes no part but the last smaller than 5 MiB.
	PartSize int64
	// MaxCopySize is the largest object Copy copies in one CopyObject (zero: MaxPutSize, S3's 5 GiB); a larger one is
	// copied part by part (UploadPartCopy).
	MaxCopySize int64
}

// New returns a store for cfg. client nil means http.DefaultClient.
func New(cfg Config, client *http.Client) (*Store, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	ep, err := url.Parse(cfg.Endpoint)
	if err != nil || (ep.Scheme != "http" && ep.Scheme != "https") || ep.Host == "" || (ep.Path != "" && ep.Path != "/") {
		return nil, fmt.Errorf("snapstore/s3: endpoint %q: want http(s)://host[:port]", cfg.Endpoint)
	}
	if cfg.Bucket == "" || strings.Contains(cfg.Bucket, "/") {
		return nil, fmt.Errorf("snapstore/s3: bucket %q", cfg.Bucket)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("snapstore/s3: no credentials (access key id and secret access key)")
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		if err := snapstore.ValidateKey(prefix); err != nil {
			return nil, fmt.Errorf("snapstore/s3: prefix: %w", err)
		}
		prefix += "/"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Store{cfg: cfg, endpoint: ep, prefix: prefix, client: client}, nil
}

// Config returns the store's configuration (defaults filled in).
func (s *Store) Config() Config { return s.cfg }

func (s *Store) objectURL(key string) (*url.URL, error) {
	if err := snapstore.ValidateKey(key); err != nil {
		return nil, err
	}
	u := *s.endpoint
	u.Path = "/" + s.cfg.Bucket + "/" + s.prefix + key
	return &u, nil
}

// do sends one signed request. body, if not nil, has size bytes whose hex SHA-256 is payloadHash.
func (s *Store) do(ctx context.Context, method, key string, hdr http.Header, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	return s.doQuery(ctx, method, key, nil, hdr, body, size, payloadHash)
}

// doQuery is do on the object's URL with query q (a multipart upload's calls).
func (s *Store) doQuery(ctx context.Context, method, key string, q url.Values, hdr http.Header, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Body, req.ContentLength = io.NopCloser(body), size
		if size == 0 {
			req.Body = http.NoBody
		}
	} else {
		payloadHash = EmptySHA256
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	Sign(req, Credentials{AccessKeyID: s.cfg.AccessKeyID, SecretAccessKey: s.cfg.SecretAccessKey, SessionToken: s.cfg.SessionToken},
		s.cfg.Region, "s3", payloadHash, now())
	return s.client.Do(req)
}

// statusError reads an S3 error answer.
func statusError(op, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	code := ""
	if i := strings.Index(string(b), "<Code>"); i >= 0 {
		if j := strings.Index(string(b[i:]), "</Code>"); j > 0 {
			code = " " + string(b[i+6:i+j])
		}
	}
	return fmt.Errorf("snapstore/s3: %s %s: HTTP %d%s", op, key, resp.StatusCode, code)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

// source gives the bytes of r twice: once to hash, once to upload. A seekable r is read in place; any other is
// spooled to a temporary file.
func source(ctx context.Context, r io.Reader) (rs io.ReadSeeker, start int64, cleanup func(), err error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		start, err := rs.Seek(0, io.SeekCurrent)
		if err == nil {
			return rs, start, func() {}, nil
		}
	}
	f, err := os.CreateTemp("", "herd-s3-put-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup = func() { f.Close(); os.Remove(f.Name()) }
	if _, err := io.Copy(f, ctxReader{ctx, r}); err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return f, 0, cleanup, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (snapstore.Object, error) {
	if _, err := s.objectURL(key); err != nil {
		return snapstore.Object{}, err
	}
	obj, err := s.put(ctx, key, r)
	if err != nil {
		// Whatever is under key now is not what this Put answers for: the outcome may be unknown (the answer was lost
		// after the endpoint stored the bytes), or an older object is still there. Remove it, with a context of its
		// own, since the caller's may be what failed.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := s.Delete(dctx, key); derr != nil {
			return snapstore.Object{}, fmt.Errorf("snapstore/s3: put %s: %w (and removing what is under the key: %v)", key, err, derr)
		}
		return snapstore.Object{}, fmt.Errorf("snapstore/s3: put %s: %w", key, err)
	}
	return obj, nil
}

func (s *Store) put(ctx context.Context, key string, r io.Reader) (snapstore.Object, error) {
	rs, start, cleanup, err := source(ctx, r)
	if err != nil {
		return snapstore.Object{}, err
	}
	defer cleanup()
	end, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return snapstore.Object{}, err
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return snapstore.Object{}, err
	}
	want := end - start
	if want > MaxObjectSize {
		return snapstore.Object{}, fmt.Errorf("%d bytes is more than S3 stores in one object (%d)", want, int64(MaxObjectSize))
	}
	part := s.partSize(want)
	h := sha256.New()
	var w io.Writer = h
	var parts *partSums
	if want > part {
		parts = &partSums{size: part}
		w = io.MultiWriter(h, parts)
	}
	size, err := io.Copy(w, ctxReader{ctx, rs})
	if err != nil {
		return snapstore.Object{}, err
	}
	if size != want {
		return snapstore.Object{}, fmt.Errorf("the source changed while it was read: %d bytes, then %d", want, size)
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return snapstore.Object{}, err
	}
	sum := h.Sum(nil)
	obj := snapstore.Object{Key: key, Digest: "sha256:" + hex.EncodeToString(sum), Size: size}
	if parts != nil {
		err = s.uploadParts(ctx, key, rs, start, obj, part, parts.done())
	} else {
		err = s.upload(ctx, key, rs, obj, sum)
	}
	if err != nil {
		return snapstore.Object{}, err
	}
	return obj, nil
}

// partSize is the part size an object of size bytes goes up in: PartSize, or larger where that would make more than
// MaxParts parts.
func (s *Store) partSize(size int64) int64 {
	part := s.PartSize
	if part <= 0 {
		part = DefaultPartSize
	}
	return max(part, (size+MaxParts-1)/MaxParts)
}

// partSums is the SHA-256 of each consecutive size bytes written to it.
type partSums struct {
	size int64
	n    int64
	h    hash.Hash
	sums [][]byte
}

func (p *partSums) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		if p.h == nil {
			p.h, p.n = sha256.New(), 0
		}
		k := min(int64(len(b)), p.size-p.n)
		p.h.Write(b[:k])
		p.n += k
		b = b[k:]
		if p.n == p.size {
			p.sums, p.h = append(p.sums, p.h.Sum(nil)), nil
		}
	}
	return total, nil
}

func (p *partSums) done() [][]byte {
	if p.h != nil {
		p.sums, p.h = append(p.sums, p.h.Sum(nil)), nil
	}
	return p.sums
}

// upload PUTs the bytes and checks that the endpoint holds exactly them.
func (s *Store) upload(ctx context.Context, key string, body io.Reader, obj snapstore.Object, sum []byte) error {
	hdr := http.Header{}
	hdr.Set(DigestHeader, obj.Digest)
	hdr.Set(ChecksumHeader, base64.StdEncoding.EncodeToString(sum))
	resp, err := s.do(ctx, http.MethodPut, key, hdr, ctxReader{ctx, io.LimitReader(body, obj.Size)}, obj.Size, hex.EncodeToString(sum))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return statusError("put", key, resp)
	}
	drain(resp)
	return s.check(ctx, key, obj, base64.StdEncoding.EncodeToString(sum))
}

// check answers whether the endpoint holds exactly obj under key: the size and herd digest it records, and the
// endpoint's own SHA-256 checksum of what it stored, which must be checksum; an endpoint that keeps none has the
// object read back.
func (s *Store) check(ctx context.Context, key string, obj snapstore.Object, checksum string) error {
	got, have, err := s.head(ctx, key)
	if err != nil {
		return fmt.Errorf("after upload: %w", err)
	}
	if got.Size != obj.Size || got.Digest != obj.Digest {
		return fmt.Errorf("%w: the endpoint holds %d bytes recorded %s, uploaded %d bytes %s", snapstore.ErrDigestMismatch, got.Size, got.Digest, obj.Size, obj.Digest)
	}
	if have != "" {
		if have != checksum {
			return fmt.Errorf("%w: the endpoint's SHA-256 of what it stored is %s, uploaded %s", snapstore.ErrDigestMismatch, have, checksum)
		}
		return nil
	}
	// No checksum kept by the endpoint: read the object back.
	if _, err := s.Verify(ctx, key, obj.Digest); err != nil {
		return fmt.Errorf("after upload: %w", err)
	}
	return nil
}

// uploadParts stores obj as a multipart upload of the parts of body (from start) whose SHA-256s are sums, each part
// bytes long but the last, and checks the endpoint holds exactly them. An upload that does not complete is aborted.
func (s *Store) uploadParts(ctx context.Context, key string, body io.ReadSeeker, start int64, obj snapstore.Object, part int64, sums [][]byte) (err error) {
	hdr := http.Header{}
	hdr.Set(DigestHeader, obj.Digest)
	hdr.Set("X-Amz-Checksum-Algorithm", "SHA256")
	id, err := s.createUpload(ctx, key, hdr)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		// The endpoint keeps the parts of an upload until it is completed or aborted, out of Stat's sight.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if aerr := s.abortUpload(actx, key, id); aerr != nil {
			err = fmt.Errorf("%w (and aborting the upload: %v)", err, aerr)
		}
	}()
	done := completeUpload{}
	all := make([]byte, 0, len(sums)*sha256.Size)
	for i, sum := range sums {
		off := int64(i) * part
		n := min(part, obj.Size-off)
		if _, err := body.Seek(start+off, io.SeekStart); err != nil {
			return err
		}
		checksum := base64.StdEncoding.EncodeToString(sum)
		phdr := http.Header{}
		phdr.Set(ChecksumHeader, checksum)
		q := url.Values{"partNumber": {strconv.Itoa(i + 1)}, "uploadId": {id}}
		resp, err := s.doQuery(ctx, http.MethodPut, key, q, phdr, ctxReader{ctx, io.LimitReader(body, n)}, n, hex.EncodeToString(sum))
		if err != nil {
			return fmt.Errorf("part %d: %w", i+1, err)
		}
		if resp.StatusCode/100 != 2 {
			defer resp.Body.Close()
			return fmt.Errorf("part %d: %w", i+1, statusError("put", key, resp))
		}
		drain(resp)
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return fmt.Errorf("part %d: the endpoint answered no ETag", i+1)
		}
		done.Parts = append(done.Parts, completedPart{PartNumber: i + 1, ETag: etag, ChecksumSHA256: checksum})
		all = append(all, sum...)
	}
	if err := s.completeUpload(ctx, key, id, done); err != nil {
		return err
	}
	completed = true
	composite := sha256.Sum256(all)
	return s.check(ctx, key, obj, base64.StdEncoding.EncodeToString(composite[:])+"-"+strconv.Itoa(len(sums)))
}

type completeUpload struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

type completedPart struct {
	PartNumber     int
	ETag           string
	ChecksumSHA256 string
}

// createUpload starts a multipart upload of key and returns its id.
func (s *Store) createUpload(ctx context.Context, key string, hdr http.Header) (string, error) {
	resp, err := s.doQuery(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, hdr, nil, 0, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", statusError("create upload", key, resp)
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("create upload %s: the endpoint answered no upload id (%v)", key, err)
	}
	return out.UploadID, nil
}

// completeUpload completes the upload with the parts. S3 may answer 200 and an error in the body, so the body is read
// for one.
func (s *Store) completeUpload(ctx context.Context, key, id string, c completeUpload) error {
	b, err := xml.Marshal(c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/xml")
	resp, err := s.doQuery(ctx, http.MethodPost, key, url.Values{"uploadId": {id}}, hdr, bytes.NewReader(b), int64(len(b)), hex.EncodeToString(sum[:]))
	if err != nil {
		return fmt.Errorf("complete upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError("complete upload", key, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("complete upload: %w", err)
	}
	if !bytes.Contains(body, []byte("<CompleteMultipartUploadResult")) {
		return fmt.Errorf("complete upload %s: the endpoint answered %d without a result: %.200s", key, resp.StatusCode, body)
	}
	return nil
}

// abortUpload aborts the upload; one the endpoint no longer knows is gone already.
func (s *Store) abortUpload(ctx context.Context, key, id string) error {
	resp, err := s.doQuery(ctx, http.MethodDelete, key, url.Values{"uploadId": {id}}, nil, nil, 0, "")
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		defer resp.Body.Close()
		return statusError("abort upload", key, resp)
	}
	drain(resp)
	return nil
}

// head reports the object and the endpoint's own SHA-256 checksum of it (base64; empty if it keeps none).
func (s *Store) head(ctx context.Context, key string) (snapstore.Object, string, error) {
	o, have, _, err := s.headRecorded(ctx, key)
	return o, have, err
}

// headRecorded is head, and the checksum recorded in the object's metadata (ChecksumMetaHeader) or "".
func (s *Store) headRecorded(ctx context.Context, key string) (o snapstore.Object, have, recorded string, err error) {
	hdr := http.Header{}
	hdr.Set("X-Amz-Checksum-Mode", "ENABLED")
	resp, err := s.do(ctx, http.MethodHead, key, hdr, nil, 0, "")
	if err != nil {
		return snapstore.Object{}, "", "", fmt.Errorf("snapstore/s3: stat %s: %w", key, err)
	}
	defer drain(resp)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return snapstore.Object{}, "", "", fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	case resp.StatusCode/100 != 2:
		return snapstore.Object{}, "", "", fmt.Errorf("snapstore/s3: stat %s: HTTP %d", key, resp.StatusCode)
	}
	o = snapstore.Object{Key: key, Digest: resp.Header.Get(DigestHeader)}
	if !strings.HasPrefix(o.Digest, "sha256:") {
		return snapstore.Object{}, "", "", fmt.Errorf("snapstore/s3: stat %s: the object has no herd digest (not written by this store)", key)
	}
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return snapstore.Object{}, "", "", fmt.Errorf("snapstore/s3: stat %s: content length: %w", key, err)
	}
	o.Size = n
	return o, resp.Header.Get(ChecksumHeader), resp.Header.Get(ChecksumMetaHeader), nil
}

// prove answers whether the object at key holds digest's bytes (and is size bytes long, unless size is negative) by
// a HEAD where the endpoint's checksum can say it: a full-object SHA-256 checksum must be the digest itself, and a
// composite one (an object made of parts) the checksum recorded with the object (ChecksumMetaHeader). An endpoint
// that keeps no checksum, or a composite one nothing was recorded for, has the object read back (Verify).
func (s *Store) prove(ctx context.Context, key, digest string, size int64) (snapstore.Object, error) {
	o, have, recorded, err := s.headRecorded(ctx, key)
	if err != nil {
		return snapstore.Object{}, err
	}
	if o.Digest != digest || (size >= 0 && o.Size != size) {
		return snapstore.Object{}, fmt.Errorf("%w: %s holds %d bytes recorded %s, want %d bytes %s", snapstore.ErrDigestMismatch, key, o.Size, o.Digest, size, digest)
	}
	composite := strings.Contains(have, "-")
	switch {
	case have == "", composite && recorded == "":
		return s.Verify(ctx, key, digest)
	case !composite && have == fullChecksum(digest), composite && have == recorded:
		return o, nil
	}
	return snapstore.Object{}, fmt.Errorf("%w: the endpoint's SHA-256 of what %s holds is %s, want %s", snapstore.ErrDigestMismatch, key, have,
		map[bool]string{false: fullChecksum(digest), true: recorded}[composite])
}

// fullChecksum is S3's full-object SHA-256 checksum (base64) of the bytes whose digest is "sha256:<hex>", or "".
func fullChecksum(digest string) string {
	sum, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(sum) != sha256.Size || !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	return base64.StdEncoding.EncodeToString(sum)
}

func (s *Store) Stat(ctx context.Context, key string) (snapstore.Object, error) {
	o, _, err := s.head(ctx, key)
	return o, err
}

func (s *Store) Verify(ctx context.Context, key, digest string) (snapstore.Object, error) {
	o, err := s.Stat(ctx, key)
	if err != nil {
		return snapstore.Object{}, err
	}
	rc, err := s.Open(ctx, key)
	if err != nil {
		return snapstore.Object{}, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, rc})
	if err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/s3: verify %s: %w", key, err)
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest || got != o.Digest || n != o.Size {
		return snapstore.Object{}, fmt.Errorf("%w: %s holds %s (%d bytes), recorded %s, want %s", snapstore.ErrDigestMismatch, key, got, n, o.Digest, digest)
	}
	return o, nil
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil, 0, "")
	if err != nil {
		if errors.Is(err, snapstore.ErrBadKey) {
			return nil, err
		}
		return nil, fmt.Errorf("snapstore/s3: open %s: %w", key, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		drain(resp)
		return nil, fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	case resp.StatusCode/100 != 2:
		defer resp.Body.Close()
		return nil, statusError("open", key, resp)
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil, 0, "")
	if err != nil {
		if errors.Is(err, snapstore.ErrBadKey) {
			return err
		}
		return fmt.Errorf("snapstore/s3: delete %s: %w", key, err)
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		defer resp.Body.Close()
		return statusError("delete", key, resp)
	}
	drain(resp)
	if _, _, err := s.head(ctx, key); !errors.Is(err, snapstore.ErrNotFound) {
		return fmt.Errorf("%w: %s still there (%v)", snapstore.ErrNotDeleted, key, err)
	}
	return nil
}

// errCopyRefused is an endpoint's answer that it does not copy objects server-side; Copy streams the object instead.
var errCopyRefused = errors.New("the endpoint does not copy objects")

// copyAnswer reads the answer to a CopyObject or UploadPartCopy into result, the element a success carries. Copy
// falls back to streaming (errCopyRefused) on an answer that says the endpoint does not implement the call — HTTP
// 501 or 405, or an error whose code is NotImplemented, MethodNotAllowed, InvalidRequest or InvalidArgument, which
// S3-compatible endpoints answer to headers they do not take — and on a success without the result element (an
// endpoint that ignored x-amz-copy-source and stored the empty body). Anything else fails the copy: a 5xx or a
// network error is retried by the caller, never hidden by a stream of the whole object. S3 may answer 200 with an
// error in the body, which fails the copy too.
func copyAnswer(op, key string, resp *http.Response, result any) error {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("snapstore/s3: %s %s: %w", op, key, err)
	}
	code := ""
	if i := bytes.Index(b, []byte("<Code>")); i >= 0 {
		if j := bytes.Index(b[i:], []byte("</Code>")); j > 0 {
			code = string(b[i+6 : i+j])
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotImplemented || resp.StatusCode == http.StatusMethodNotAllowed,
		resp.StatusCode/100 == 4 && (code == "NotImplemented" || code == "MethodNotAllowed" || code == "InvalidRequest" || code == "InvalidArgument"):
		return fmt.Errorf("%w: %s %s: HTTP %d %s", errCopyRefused, op, key, resp.StatusCode, code)
	case resp.StatusCode == http.StatusNotFound && code == "NoSuchKey":
		return fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	case resp.StatusCode/100 != 2 || code != "":
		return fmt.Errorf("snapstore/s3: %s %s: HTTP %d %s", op, key, resp.StatusCode, code)
	}
	if err := xml.Unmarshal(b, result); err != nil {
		return fmt.Errorf("%w: %s %s: HTTP %d without a result: %.200s", errCopyRefused, op, key, resp.StatusCode, b)
	}
	return nil
}

// copySource is the x-amz-copy-source of key.
func (s *Store) copySource(key string) string {
	return "/" + s.cfg.Bucket + "/" + uriEncode(s.prefix+key, false)
}

// Copy copies the object inside the bucket, so none of its bytes pass through herd: one CopyObject up to
// MaxCopySize (S3's 5 GiB), and UploadPartCopy of the source's own parts past it. The copy's metadata is the herd
// digest; it is proved as Prove proves an object, by a HEAD: the endpoint's SHA-256 of what a CopyObject stored is
// the digest itself, and the composite checksum of a part copy, made of the source's parts, is the source's; only an
// endpoint that keeps no checksum has the copy read. An endpoint that does not copy objects (see copyAnswer) has the
// object streamed through herd instead (snapstore.StreamCopy). A Copy that fails leaves nothing under to.
func (s *Store) Copy(ctx context.Context, from, to, digest string) (snapstore.Object, error) {
	if _, err := s.objectURL(to); err != nil {
		return snapstore.Object{}, err
	}
	src, sum, err := s.head(ctx, from)
	if err != nil {
		return snapstore.Object{}, err
	}
	if src.Digest != digest {
		return snapstore.Object{}, fmt.Errorf("%w: %s records %s, want %s", snapstore.ErrDigestMismatch, from, src.Digest, digest)
	}
	obj, err := s.copy(ctx, from, to, src, sum)
	if err == nil {
		return obj, nil
	}
	// As in Put: whatever is under to is not what this Copy answers for.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if derr := s.Delete(dctx, to); derr != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/s3: copy %s to %s: %w (and removing what is under the key: %v)", from, to, err, derr)
	}
	if errors.Is(err, errCopyRefused) {
		return snapstore.StreamCopy(ctx, s, from, to, digest)
	}
	return snapstore.Object{}, fmt.Errorf("snapstore/s3: copy %s to %s: %w", from, to, err)
}

func (s *Store) copy(ctx context.Context, from, to string, src snapstore.Object, sum string) (snapstore.Object, error) {
	maxCopy := s.MaxCopySize
	if maxCopy <= 0 {
		maxCopy = MaxPutSize
	}
	if src.Size <= maxCopy {
		if err := s.copyObject(ctx, from, to, src.Digest); err != nil {
			return snapstore.Object{}, err
		}
		return s.prove(ctx, to, src.Digest, src.Size)
	}
	part, composite, err := s.sourceParts(ctx, from, src.Size, sum)
	if err != nil {
		return snapstore.Object{}, err
	}
	if part > maxCopy {
		return snapstore.Object{}, fmt.Errorf("%w: %s's parts are %d bytes, more than one copy takes (%d)", errCopyRefused, from, part, maxCopy)
	}
	if err := s.copyParts(ctx, from, to, src, part, composite); err != nil {
		return snapstore.Object{}, err
	}
	return s.prove(ctx, to, src.Digest, src.Size)
}

// copyObject is one CopyObject of from to to, with the herd digest as the copy's metadata and a SHA-256 checksum of
// the bytes the endpoint stores.
func (s *Store) copyObject(ctx context.Context, from, to, digest string) error {
	hdr := http.Header{}
	hdr.Set("X-Amz-Copy-Source", s.copySource(from))
	hdr.Set("X-Amz-Metadata-Directive", "REPLACE")
	hdr.Set(DigestHeader, digest)
	hdr.Set("X-Amz-Checksum-Algorithm", "SHA256")
	resp, err := s.do(ctx, http.MethodPut, to, hdr, nil, 0, "")
	if err != nil {
		return err
	}
	var out struct {
		XMLName xml.Name `xml:"CopyObjectResult"`
	}
	return copyAnswer("copy", to, resp, &out)
}

// sourceParts is the part size the source was uploaded in, from a HEAD of its first part, and its composite checksum
// when a copy in those parts can be proved by it (empty otherwise: the source is not a multipart object of parts of
// that size, or the endpoint reports no checksum).
func (s *Store) sourceParts(ctx context.Context, key string, size int64, sum string) (int64, string, error) {
	hdr := http.Header{}
	hdr.Set("X-Amz-Checksum-Mode", "ENABLED")
	resp, err := s.doQuery(ctx, http.MethodHead, key, url.Values{"partNumber": {"1"}}, hdr, nil, 0, "")
	if err != nil {
		return 0, "", fmt.Errorf("snapstore/s3: stat %s part 1: %w", key, err)
	}
	drain(resp)
	if resp.StatusCode/100 != 2 {
		return 0, "", fmt.Errorf("snapstore/s3: stat %s part 1: HTTP %d", key, resp.StatusCode)
	}
	part, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	n, nerr := strconv.Atoi(resp.Header.Get("X-Amz-Mp-Parts-Count"))
	if err != nil || nerr != nil || part <= 0 || int64(n) != (size+part-1)/part || !strings.HasSuffix(sum, "-"+strconv.Itoa(n)) {
		// Not parts this copy can repeat: copy in parts of the store's own size, and prove the copy by reading it.
		return s.partSize(size), "", nil
	}
	return part, sum, nil
}

// copyParts copies from to to as a multipart upload of UploadPartCopy calls of part bytes each but the last, and
// records composite (the source's composite checksum, if the parts are the source's) as the checksum the copy must
// have. An upload that does not complete is aborted.
func (s *Store) copyParts(ctx context.Context, from, to string, src snapstore.Object, part int64, composite string) (err error) {
	hdr := http.Header{}
	hdr.Set(DigestHeader, src.Digest)
	if composite != "" {
		hdr.Set(ChecksumMetaHeader, composite)
	}
	hdr.Set("X-Amz-Checksum-Algorithm", "SHA256")
	id, err := s.createUpload(ctx, to, hdr)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if aerr := s.abortUpload(actx, to, id); aerr != nil {
			err = fmt.Errorf("%w (and aborting the upload: %v)", err, aerr)
		}
	}()
	done := completeUpload{}
	for i, off := 0, int64(0); off < src.Size; i, off = i+1, off+part {
		phdr := http.Header{}
		phdr.Set("X-Amz-Copy-Source", s.copySource(from))
		phdr.Set("X-Amz-Copy-Source-Range", fmt.Sprintf("bytes=%d-%d", off, min(off+part, src.Size)-1))
		q := url.Values{"partNumber": {strconv.Itoa(i + 1)}, "uploadId": {id}}
		resp, err := s.doQuery(ctx, http.MethodPut, to, q, phdr, nil, 0, "")
		if err != nil {
			return fmt.Errorf("part %d: %w", i+1, err)
		}
		var out struct {
			XMLName        xml.Name `xml:"CopyPartResult"`
			ETag           string
			ChecksumSHA256 string
		}
		if err := copyAnswer("copy part", to, resp, &out); err != nil {
			return fmt.Errorf("part %d: %w", i+1, err)
		}
		if out.ETag == "" {
			return fmt.Errorf("part %d: the endpoint answered no ETag", i+1)
		}
		done.Parts = append(done.Parts, completedPart{PartNumber: i + 1, ETag: out.ETag, ChecksumSHA256: out.ChecksumSHA256})
	}
	if err := s.completeUpload(ctx, to, id, done); err != nil {
		return err
	}
	completed = true
	return nil
}
