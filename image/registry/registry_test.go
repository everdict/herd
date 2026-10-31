package registry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// server is a registry that wants a bearer token, serving one two-platform index under team/tool:1.
type server struct {
	blobs     map[string][]byte
	manifests map[string][]byte
	types     map[string]string
	tags      map[string]string
	tokens    int
}

func gz(b []byte) []byte {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write(b)
	_ = w.Close()
	return z.Bytes()
}

func (s *server) add(body []byte, mt string) string {
	d := image.DigestOf(body)
	if mt == "" {
		s.blobs[d] = body
	} else {
		s.manifests[d], s.types[d] = body, mt
	}
	return d
}

func (s *server) platformManifest(arch string) (string, int) {
	cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{"Cmd": []string{"/bin/" + arch}}})
	layer := gz(imagetest.Layer(imagetest.File{Name: "arch", Body: arch}))
	m, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mediaOCIManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": s.add(cfg, ""), "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": s.add(layer, ""), "size": len(layer)}},
	})
	return s.add(m, mediaOCIManifest), len(m)
}

func newServer() (*server, string) {
	s := &server{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{}}
	var entries []any
	for _, arch := range []string{"amd64", "arm64"} {
		d, n := s.platformManifest(arch)
		entries = append(entries, map[string]any{"mediaType": mediaOCIManifest, "digest": d, "size": n, "platform": map[string]string{"os": "linux", "architecture": arch}})
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex, "manifests": entries})
	d := s.add(idx, mediaOCIIndex)
	s.tags["1"] = d
	return s, d
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		if r.URL.Query().Get("scope") != "repository:team/tool:pull" || r.URL.Query().Get("service") != "test-registry" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		s.tokens++
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "t0k"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer t0k" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="test-registry",scope="repository:team/tool:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/team/tool/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	switch kind {
	case "manifests":
		if d, ok := s.tags[ref]; ok {
			ref = d
		}
		b, ok := s.manifests[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", s.types[ref])
		_, _ = w.Write(b)
	case "blobs":
		b, ok := s.blobs[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func TestPullThroughCache(t *testing.T) {
	s, idx := newServer()
	ts := httptest.NewServer(s)
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	c := &Client{PlainHTTP: func(string) bool { return true }}
	cache := &image.Cache{Dir: t.TempDir(), Source: c, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "arm64"}}
	for _, ref := range []string{host + "/team/tool:1", host + "/team/tool@" + idx} {
		r, err := cache.Get(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(r.Path)
		if string(b) != "-0644 0:0 5 arch\n" || len(r.Config.Cmd) != 1 || r.Config.Cmd[0] != "/bin/arm64" || r.Digest == idx {
			t.Fatalf("%s: rootfs %+v holds %q", ref, r, b)
		}
	}
	if s.tokens != 1 {
		t.Errorf("%d tokens fetched, want 1 (reused)", s.tokens)
	}
}

func TestManifestDigestChecked(t *testing.T) {
	s, idx := newServer()
	// The registry answers other bytes under the index's digest.
	s.manifests[idx] = append(append([]byte(nil), s.manifests[idx]...), ' ')
	ts := httptest.NewServer(s)
	defer ts.Close()
	ref, err := image.ParseRef(strings.TrimPrefix(ts.URL, "http://") + "/team/tool@" + idx)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{PlainHTTP: func(string) bool { return true }}
	if _, err := c.Resolve(context.Background(), ref, image.Platform{OS: "linux", Architecture: "amd64"}); !errors.Is(err, image.ErrDigest) {
		t.Fatalf("err = %v, want ErrDigest", err)
	}
}

func TestNoPlatform(t *testing.T) {
	s, _ := newServer()
	ts := httptest.NewServer(s)
	defer ts.Close()
	ref, _ := image.ParseRef(strings.TrimPrefix(ts.URL, "http://") + "/team/tool:1")
	c := &Client{PlainHTTP: func(string) bool { return true }}
	if _, err := c.Resolve(context.Background(), ref, image.Platform{OS: "linux", Architecture: "riscv64"}); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// pinned is a two-platform image on an imagetest.Server, reached through a client whose waits are recorded.
func pinned(t *testing.T) (*imagetest.Server, image.Ref, *Client, *[]time.Duration) {
	s := imagetest.NewServer()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	var ms []imagetest.Manifest
	for _, arch := range []string{"amd64", "arm64"} {
		ms = append(ms, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: arch},
			Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "arch", Body: arch})}})
	}
	s.Push("team/tool", "1", ms...)
	ref, err := image.ParseRef(strings.TrimPrefix(ts.URL, "http://") + "/team/tool:1")
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	c := &Client{sleep: func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}}
	return s, ref, c, &waits
}

// TestRetryAfterHonoured: a 429 or 503 is retried after the registry's Retry-After (seconds or an HTTP date), or
// after a doubling backoff from a second without one, and the pull then succeeds. Today the first 429 fails it.
func TestRetryAfterHonoured(t *testing.T) {
	s, ref, c, waits := pinned(t)
	s.Refuse(imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: "7"},
		imagetest.Answer{Status: http.StatusServiceUnavailable},
		imagetest.Answer{Status: http.StatusTooManyRequests},
		imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)})
	img, err := c.Resolve(context.Background(), ref, image.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("Resolve after four refusals: %v", err)
	}
	if !image.ValidDigest(img.Index) || img.Index == img.Digest {
		t.Errorf("index %q, platform digest %q", img.Index, img.Digest)
	}
	want := []time.Duration{7 * time.Second, time.Second, 2 * time.Second, 0}
	if len(*waits) != len(want) {
		t.Fatalf("waited %v, want %v", *waits, want)
	}
	for i := range want {
		if (*waits)[i] != want[i] {
			t.Errorf("wait %d: %v, want %v", i, (*waits)[i], want[i])
		}
	}
}

// TestRefusalIsUnavailable: a registry that keeps refusing answers a typed unavailable after the bounded retries,
// and so does a Retry-After past MaxWait or past the context's deadline, without waiting for it. Today the first
// 429 is a generic error.
func TestRefusalIsUnavailable(t *testing.T) {
	arm := image.Platform{OS: "linux", Architecture: "arm64"}
	s, ref, c, waits := pinned(t)
	for range 10 {
		s.Refuse(imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: "1"})
	}
	_, err := c.Resolve(context.Background(), ref, arm)
	var u *UnavailableError
	if !errors.Is(err, image.ErrUnavailable) || !errors.As(err, &u) || u.Attempts != DefaultRetries+1 || u.RetryAfter != time.Second {
		t.Fatalf("err = %v (%+v), want ErrUnavailable after %d attempts", err, u, DefaultRetries+1)
	}
	if len(*waits) != DefaultRetries {
		t.Errorf("waited %v, want %d waits", *waits, DefaultRetries)
	}

	s, ref, c, waits = pinned(t)
	s.Refuse(imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: "3600"})
	if _, err := c.Resolve(context.Background(), ref, arm); !errors.As(err, &u) || u.RetryAfter != time.Hour || len(*waits) != 0 {
		t.Fatalf("Retry-After past MaxWait: err = %v, waits %v; want ErrUnavailable at once", err, *waits)
	}

	s, ref, c, waits = pinned(t)
	s.Refuse(imagetest.Answer{Status: http.StatusServiceUnavailable, RetryAfter: "5"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Resolve(ctx, ref, arm); !errors.Is(err, image.ErrUnavailable) || len(*waits) != 0 {
		t.Fatalf("Retry-After past the deadline: err = %v, waits %v; want ErrUnavailable at once", err, *waits)
	}
}

func TestRetryAfterForms(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"0": 0, "120": 2 * time.Minute, now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat): 0,
	} {
		if got, ok := retryAfter(v, now); !ok || got != want {
			t.Errorf("retryAfter(%q) = %v, %v; want %v", v, got, ok, want)
		}
	}
	for _, v := range []string{"", "soon", "-1", "1.5"} {
		if _, ok := retryAfter(v, now); ok {
			t.Errorf("retryAfter(%q) read as a wait", v)
		}
	}
}
