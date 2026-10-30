// Package registry is the image.Source that pulls from an OCI distribution registry over HTTP (Docker Hub, GHCR, a
// local registry): manifests and indexes by tag or digest, anonymous bearer tokens on a 401 challenge, and every
// manifest checked against the digest it was asked for. Blob digests are checked by the image cache.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/everdict/herd/image"
)

const (
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	maxManifest         = 4 << 20
)

// Client pulls from registries.
type Client struct {
	HTTP *http.Client
	// PlainHTTP says which registry hosts are spoken to without TLS; default: localhost and 127.0.0.1 only.
	PlainHTTP func(host string) bool

	mu     sync.Mutex
	tokens map[string]string // registry + repository → bearer token
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) base(host string) string {
	plain := c.PlainHTTP
	if plain == nil {
		plain = func(h string) bool {
			name := h
			if i := strings.LastIndexByte(h, ':'); i >= 0 {
				name = h[:i]
			}
			return name == "localhost" || name == "127.0.0.1"
		}
	}
	if plain(host) {
		return "http://" + host
	}
	return "https://" + host
}

type manifest struct {
	MediaType string             `json:"mediaType"`
	Config    image.Descriptor   `json:"config"`
	Layers    []image.Descriptor `json:"layers"`
	Manifests []struct {
		image.Descriptor
		Platform image.Platform `json:"platform"`
	} `json:"manifests"`
}

// Resolve fetches the manifest ref names and, for an index, the platform's manifest, then the image config.
func (c *Client) Resolve(ctx context.Context, ref image.Ref, p image.Platform) (image.Image, error) {
	want := ref.Digest
	if want == "" {
		want = ref.Tag
	}
	b, mt, err := c.fetchManifest(ctx, ref, want)
	if err != nil {
		return image.Image{}, err
	}
	digest := image.DigestOf(b)
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return image.Image{}, fmt.Errorf("registry: manifest %s: %w", ref, err)
	}
	if mt == "" {
		mt = m.MediaType
	}
	if mt == mediaOCIIndex || mt == mediaDockerList {
		var pick string
		for _, e := range m.Manifests {
			if e.Platform.OS == p.OS && e.Platform.Architecture == p.Architecture && (p.Variant == "" || e.Platform.Variant == p.Variant) {
				pick = e.Digest
				break
			}
		}
		if pick == "" {
			return image.Image{}, fmt.Errorf("registry: %s has no manifest for %s: %w", ref, p, image.ErrNotFound)
		}
		if b, _, err = c.fetchManifest(ctx, ref, pick); err != nil {
			return image.Image{}, err
		}
		digest = image.DigestOf(b)
		m = manifest{}
		if err := json.Unmarshal(b, &m); err != nil {
			return image.Image{}, fmt.Errorf("registry: manifest %s: %w", pick, err)
		}
	}
	if len(m.Layers) == 0 || !image.ValidDigest(m.Config.Digest) {
		return image.Image{}, fmt.Errorf("registry: %s: %w: manifest %s has no layers or no config", ref, image.ErrUnsupported, digest)
	}
	rc, err := c.Blob(ctx, ref, m.Config)
	if err != nil {
		return image.Image{}, err
	}
	defer rc.Close()
	cb, err := io.ReadAll(io.LimitReader(rc, maxManifest))
	if err != nil {
		return image.Image{}, fmt.Errorf("registry: config %s: %w", m.Config.Digest, err)
	}
	if got := image.DigestOf(cb); got != m.Config.Digest {
		return image.Image{}, fmt.Errorf("%w: config %s read as %s", image.ErrDigest, m.Config.Digest, got)
	}
	cfg, err := image.ParseConfig(cb)
	if err != nil {
		return image.Image{}, err
	}
	return image.Image{Digest: digest, Config: cfg, Layers: m.Layers}, nil
}

// fetchManifest GETs a manifest by tag or digest; one fetched by digest must hash to it.
func (c *Client) fetchManifest(ctx context.Context, ref image.Ref, tagOrDigest string) ([]byte, string, error) {
	resp, err := c.get(ctx, ref, "/manifests/"+tagOrDigest,
		strings.Join([]string{mediaOCIIndex, mediaOCIManifest, mediaDockerList, mediaDockerManifest}, ", "))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest))
	if err != nil {
		return nil, "", fmt.Errorf("registry: manifest %s: %w", ref, err)
	}
	if image.ValidDigest(tagOrDigest) && image.DigestOf(b) != tagOrDigest {
		return nil, "", fmt.Errorf("%w: manifest %s read as %s", image.ErrDigest, tagOrDigest, image.DigestOf(b))
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return b, strings.TrimSpace(mt), nil
}

// Blob GETs a blob (redirects to a CDN are followed; the token is not sent to another host).
func (c *Client) Blob(ctx context.Context, ref image.Ref, d image.Descriptor) (io.ReadCloser, error) {
	if !image.ValidDigest(d.Digest) {
		return nil, fmt.Errorf("%w: blob digest %q", image.ErrDigest, d.Digest)
	}
	resp, err := c.get(ctx, ref, "/blobs/"+d.Digest, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) get(ctx context.Context, ref image.Ref, path, accept string) (*http.Response, error) {
	u := c.base(ref.Registry) + "/v2/" + ref.Repository + path
	key := ref.Registry + "/" + ref.Repository
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		c.mu.Lock()
		tok := c.tokens[key]
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry: GET %s: %w", u, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			chal := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, err := c.token(ctx, chal, ref)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			if c.tokens == nil {
				c.tokens = map[string]string{}
			}
			c.tokens[key] = tok
			c.mu.Unlock()
			continue
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, fmt.Errorf("registry: GET %s: %w", u, image.ErrNotFound)
		}
		resp.Body.Close()
		return nil, fmt.Errorf("registry: GET %s: %s", u, resp.Status)
	}
}

// token answers a Bearer challenge with an anonymous pull token.
func (c *Client) token(ctx context.Context, challenge string, ref image.Ref) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry: %s: unsupported challenge %q", ref, challenge)
	}
	p := map[string]string{}
	for _, kv := range splitParams(params) {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Scheme == "" {
		return "", fmt.Errorf("registry: %s: challenge realm %q", ref, p["realm"])
	}
	q := realm.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("registry: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry: token for %s: %s", ref, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("registry: token: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", fmt.Errorf("registry: token for %s: empty", ref)
	}
	return t.Token, nil
}

// splitParams splits auth-params at commas outside quotes.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}
