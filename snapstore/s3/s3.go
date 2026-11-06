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
	hdr := http.Header{}
	hdr.Set("X-Amz-Checksum-Mode", "ENABLED")
	resp, err := s.do(ctx, http.MethodHead, key, hdr, nil, 0, "")
	if err != nil {
		return snapstore.Object{}, "", fmt.Errorf("snapstore/s3: stat %s: %w", key, err)
	}
	defer drain(resp)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return snapstore.Object{}, "", fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	case resp.StatusCode/100 != 2:
		return snapstore.Object{}, "", fmt.Errorf("snapstore/s3: stat %s: HTTP %d", key, resp.StatusCode)
	}
	o := snapstore.Object{Key: key, Digest: resp.Header.Get(DigestHeader)}
	if !strings.HasPrefix(o.Digest, "sha256:") {
		return snapstore.Object{}, "", fmt.Errorf("snapstore/s3: stat %s: the object has no herd digest (not written by this store)", key)
	}
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return snapstore.Object{}, "", fmt.Errorf("snapstore/s3: stat %s: content length: %w", key, err)
	}
	o.Size = n
	return o, resp.Header.Get(ChecksumHeader), nil
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
