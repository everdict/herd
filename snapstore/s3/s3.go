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
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/everdict/herd/snapstore"
)

// MaxObjectSize is the largest object one S3 PUT takes (5 GiB); Put refuses larger ones.
const MaxObjectSize = 5 << 30

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
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
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
	h := sha256.New()
	size, err := io.Copy(h, ctxReader{ctx, rs})
	if err != nil {
		return snapstore.Object{}, err
	}
	if size > MaxObjectSize {
		return snapstore.Object{}, fmt.Errorf("%d bytes is more than one PUT takes (%d)", size, int64(MaxObjectSize))
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return snapstore.Object{}, err
	}
	sum := h.Sum(nil)
	obj := snapstore.Object{Key: key, Digest: "sha256:" + hex.EncodeToString(sum), Size: size}
	if err := s.upload(ctx, key, rs, obj, sum); err != nil {
		return snapstore.Object{}, err
	}
	return obj, nil
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
	got, checksum, err := s.head(ctx, key)
	if err != nil {
		return fmt.Errorf("after upload: %w", err)
	}
	if got.Size != obj.Size || got.Digest != obj.Digest {
		return fmt.Errorf("%w: the endpoint holds %d bytes recorded %s, uploaded %d bytes %s", snapstore.ErrDigestMismatch, got.Size, got.Digest, obj.Size, obj.Digest)
	}
	if checksum != "" {
		if want := base64.StdEncoding.EncodeToString(sum); checksum != want {
			return fmt.Errorf("%w: the endpoint's SHA-256 of what it stored is %s, uploaded %s", snapstore.ErrDigestMismatch, checksum, want)
		}
		return nil
	}
	// No checksum kept by the endpoint: read the object back.
	if _, err := s.Verify(ctx, key, obj.Digest); err != nil {
		return fmt.Errorf("after upload: %w", err)
	}
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
