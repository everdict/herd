package imagetest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/everdict/herd/image"
)

const (
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
)

// Server is an OCI distribution registry spoken to over HTTP (mount it on an httptest.Server), for tests of the real
// registry client: what is pushed is served as OCI indexes, manifests and blobs, every request is counted by kind,
// and the next requests can be answered with a status of the test's choice (a 429 with a Retry-After).
type Server struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	types     map[string]string
	tags      map[string]string
	counts    Requests
	fail      []Answer
}

// Requests counts what a Server was asked.
type Requests struct {
	// TagManifests are manifest GETs by tag: tag resolves.
	TagManifests int
	// DigestManifests are manifest GETs by digest.
	DigestManifests int
	Blobs           int
	// Refused are requests answered with a queued Answer instead.
	Refused int
}

// Total is every request the server was sent.
func (r Requests) Total() int { return r.TagManifests + r.DigestManifests + r.Blobs + r.Refused }

// Answer is a status the server answers a request with instead of serving it.
type Answer struct {
	Status int
	// RetryAfter is the Retry-After header's value, if any.
	RetryAfter string
}

// NewServer returns an empty registry.
func NewServer() *Server {
	return &Server{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{}}
}

func (s *Server) add(body []byte, mt string) string {
	d := image.DigestOf(body)
	if mt == "" {
		s.blobs[d] = body
	} else {
		s.manifests[d], s.types[d] = body, mt
	}
	return d
}

// Push stores the manifests under repo:tag (moving the tag if it existed) and returns the digest the tag now names:
// the manifest's for one manifest, an index's for several.
func (s *Server) Push(repo, tag string, ms ...Manifest) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []any
	var last string
	for _, m := range ms {
		cfg, _ := json.Marshal(map[string]any{"architecture": m.Platform.Architecture, "os": m.Platform.OS, "config": m.Config})
		var layers []any
		for _, l := range m.Layers {
			var z bytes.Buffer
			zw := gzip.NewWriter(&z)
			_, _ = zw.Write(l)
			_ = zw.Close()
			layers = append(layers, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": s.add(z.Bytes(), ""), "size": z.Len()})
		}
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": mediaManifest,
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": s.add(cfg, ""), "size": len(cfg)},
			"layers": layers,
		})
		last = s.add(body, mediaManifest)
		p := map[string]string{"os": m.Platform.OS, "architecture": m.Platform.Architecture}
		if m.Platform.Variant != "" {
			p["variant"] = m.Platform.Variant
		}
		entries = append(entries, map[string]any{"mediaType": mediaManifest, "digest": last, "size": len(body), "platform": p})
	}
	d := last
	if len(ms) > 1 {
		idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": entries})
		d = s.add(idx, mediaIndex)
	}
	s.tags[repo+":"+tag] = d
	return d
}

// Refuse queues answers for the next requests, one each, before any is served again.
func (s *Server) Refuse(as ...Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, as...)
}

// Requests is what the server was asked so far.
func (s *Server) Requests() Requests {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fail) > 0 {
		a := s.fail[0]
		s.fail = s.fail[1:]
		s.counts.Refused++
		if a.RetryAfter != "" {
			w.Header().Set("Retry-After", a.RetryAfter)
		}
		http.Error(w, http.StatusText(a.Status), a.Status)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	var repo, kind, ref string
	for _, k := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(rest, k); i >= 0 {
			repo, kind, ref = rest[:i], strings.Trim(k, "/"), rest[i+len(k):]
			break
		}
	}
	switch kind {
	case "manifests":
		if image.ValidDigest(ref) {
			s.counts.DigestManifests++
		} else {
			s.counts.TagManifests++
			ref = s.tags[repo+":"+ref]
		}
		b, ok := s.manifests[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", s.types[ref])
		_, _ = w.Write(b)
	case "blobs":
		s.counts.Blobs++
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
