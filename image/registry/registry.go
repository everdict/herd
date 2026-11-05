// Package registry is the image.Source that pulls from an OCI distribution registry over HTTP (Docker Hub, GHCR, a
// local registry): manifests and indexes by tag or digest, a 401 challenge answered with a bearer token (anonymous, or
// for the node's login to that registry, Credentials) or a Basic login, and every manifest checked against the digest
// it was asked for. Blob digests are checked by the image cache. A 429 or 503 is retried after its Retry-After (or a
// doubling backoff), a bounded number of times and never past the request's deadline, and then answers
// UnavailableError (image.ErrUnavailable).
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// PlainHTTP says which registry hosts are spoken to without TLS; default: localhost and 127.0.0.1 only
	// (Loopback). PlainHosts adds the operator's insecure registries to those.
	PlainHTTP func(host string) bool

	// Retries is how often a request the registry refused with 429 or 503 is sent again before it answers
	// ErrUnavailable (default DefaultRetries; negative: never).
	Retries int
	// MaxWait bounds each wait before a retry (default DefaultMaxWait). The wait is the registry's Retry-After, or a
	// doubling backoff from one second without one; a Retry-After longer than MaxWait is not waited for, and the
	// request answers ErrUnavailable at once, carrying it; so does one that the request context's deadline would cut.
	MaxWait time.Duration
	// Credentials are the node's registry logins (PLT-222): a challenge from a registry they hold one for is answered
	// with it (Basic to the Bearer realm, or to a Basic-only registry); nil, or no login for the host: anonymous.
	Credentials Credentials

	mu     sync.Mutex
	tokens map[string]string // registry + repository → Authorization header value
	// sleep waits d unless ctx ends first; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// Defaults of Client.Retries and Client.MaxWait: a refused pull waits at most about a minute in all.
const (
	DefaultRetries = 4
	DefaultMaxWait = 30 * time.Second
)

// UnavailableError is answered for a request the registry kept refusing with 429 or 503 (image.ErrUnavailable).
type UnavailableError struct {
	URL    string
	Status string
	// RetryAfter is the registry's last Retry-After (0 when it sent none).
	RetryAfter time.Duration
	// Attempts is how often the request was sent.
	Attempts int
}

func (e *UnavailableError) Error() string {
	s := fmt.Sprintf("registry: GET %s: %s after %d attempts", e.URL, e.Status, e.Attempts)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %v)", e.RetryAfter)
	}
	return s
}

func (e *UnavailableError) Unwrap() error { return image.ErrUnavailable }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) plain(host string) bool {
	if c.PlainHTTP != nil {
		return c.PlainHTTP(host)
	}
	return Loopback(host)
}

// Loopback says whether a registry host[:port] is localhost or 127.0.0.1, spoken to without TLS by default.
func Loopback(host string) bool {
	name := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name = host[:i]
	}
	return name == "localhost" || name == "127.0.0.1"
}

// PlainHosts is a Client.PlainHTTP that speaks plain HTTP to the loopback hosts and to exactly the named registry
// hosts, as the docker daemon's insecure-registries does (PLT-224): a name is a host[:port] matched as written (case
// aside), so "10.0.0.1:5000" makes neither "10.0.0.1" nor "10.0.0.1:5001" plain. Every other host stays https.
func PlainHosts(hosts []string) func(host string) bool {
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		set[strings.ToLower(h)] = true
	}
	return func(host string) bool { return Loopback(host) || set[strings.ToLower(host)] }
}

func (c *Client) base(host string) string {
	if c.plain(host) {
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
	index := digest
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
	return image.Image{Digest: digest, Index: index, Config: cfg, Layers: m.Layers}, nil
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
	refused, backoff, authed := 0, time.Second, false
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		tok := c.cachedToken(ctx, key)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry: GET %s: %w", u, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			refused++
			after, told := retryAfter(resp.Header.Get("Retry-After"), time.Now())
			resp.Body.Close()
			wait := backoff
			if told {
				wait = after
			} else {
				backoff *= 2
			}
			// A wait the caller's deadline would cut is not begun: the caller hears why now, not that it ran out.
			deadline, bounded := ctx.Deadline()
			if refused > c.retries() || wait > c.maxWait() || bounded && time.Until(deadline) <= wait {
				return nil, &UnavailableError{URL: u, Status: resp.Status, RetryAfter: after, Attempts: refused}
			}
			if err := c.wait(ctx, wait); err != nil {
				return nil, fmt.Errorf("registry: GET %s: waiting %v after %s: %w", u, wait, resp.Status, err)
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized && !authed:
			authed = true
			chal := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, err := c.authorize(ctx, chal, ref)
			if err != nil {
				return nil, err
			}
			c.keepToken(ctx, key, tok)
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close()
			return nil, fmt.Errorf("registry: GET %s: %s with the authorization its challenge asked for", u, resp.Status)
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, fmt.Errorf("registry: GET %s: %w", u, image.ErrNotFound)
		}
		resp.Body.Close()
		return nil, fmt.Errorf("registry: GET %s: %s", u, resp.Status)
	}
}

// cachedToken is the Authorization value a pull of key earned before: in ctx's logins (WithLogins) when it carries
// some, in the Client otherwise.
func (c *Client) cachedToken(ctx context.Context, key string) string {
	mu, tokens := &c.mu, &c.tokens
	if p := loginsOf(ctx); p != nil {
		mu, tokens = &p.mu, &p.tokens
	}
	mu.Lock()
	defer mu.Unlock()
	return (*tokens)[key]
}

func (c *Client) keepToken(ctx context.Context, key, tok string) {
	mu, tokens := &c.mu, &c.tokens
	if p := loginsOf(ctx); p != nil {
		mu, tokens = &p.mu, &p.tokens
	}
	mu.Lock()
	defer mu.Unlock()
	if *tokens == nil {
		*tokens = map[string]string{}
	}
	(*tokens)[key] = tok
}

// authorize answers a 401's challenge with the Authorization header value to send again: a Bearer token from the
// challenge's realm, or Basic credentials for a Basic-only registry. Either uses the pull's own login to the registry
// (WithLogins) if it carries one, else the node's (Credentials) if it holds one; a Bearer token is fetched
// anonymously without.
func (c *Client) authorize(ctx context.Context, challenge string, ref image.Ref) (string, error) {
	scheme, params, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	var login Credential
	has := false
	whose := "this node's login"
	if p := loginsOf(ctx); p != nil && p.creds != nil {
		var err error
		if login, has, err = p.creds.Lookup(ref.Registry); err != nil {
			return "", fmt.Errorf("registry: %s: %w", ref, err)
		}
		if has {
			whose = p.what
		}
	}
	if !has && c.Credentials != nil {
		var err error
		if login, has, err = c.Credentials.Lookup(ref.Registry); err != nil {
			return "", fmt.Errorf("registry: %s: %w", ref, err)
		}
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		tok, err := c.token(ctx, params, ref, login, has, whose)
		if err != nil {
			return "", err
		}
		return "Bearer " + tok, nil
	case strings.EqualFold(scheme, "Basic"):
		if !has || login.Username == "" {
			return "", fmt.Errorf("registry: %s: the registry asks for a Basic login and this node has no username and password for %s (herdlet -registry-auth)", ref, ref.Registry)
		}
		// Sent to the registry itself: https unless PlainHTTP allows the host.
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(login.Username+":"+login.Password)), nil
	}
	return "", fmt.Errorf("registry: %s: unsupported challenge %q", ref, scheme)
}

// token answers a Bearer challenge's params with a pull token: anonymous without a login; with one, the realm is
// sent the user and password as Basic credentials (a GET), or the identity token as an OAuth2 refresh token (a POST,
// distribution's token spec). A login is sent only to an https realm, or a plain one PlainHTTP allows.
func (c *Client) token(ctx context.Context, params string, ref image.Ref, login Credential, has bool, whose string) (string, error) {
	p := map[string]string{}
	for _, kv := range splitParams(params) {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Scheme == "" {
		return "", fmt.Errorf("registry: %s: challenge realm %q", ref, p["realm"])
	}
	if has && realm.Scheme != "https" && !c.plain(realm.Host) {
		return "", fmt.Errorf("registry: %s: the challenge's realm %s is not https: the login is not sent to it", ref, realm.Redacted())
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
	var req *http.Request
	if has && login.IdentityToken != "" {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {login.IdentityToken}, "client_id": {"herd"}, "scope": {scope}}
		if p["service"] != "" {
			form.Set("service", p["service"])
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm.String(), strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		if has {
			q.Set("account", login.Username)
		}
		realm.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
		if err == nil && has {
			req.SetBasicAuth(login.Username, login.Password)
		}
	}
	if err != nil {
		return "", fmt.Errorf("registry: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			after, _ := retryAfter(resp.Header.Get("Retry-After"), time.Now())
			return "", &UnavailableError{URL: realm.Redacted(), Status: resp.Status, RetryAfter: after, Attempts: 1}
		}
		if !has {
			return "", fmt.Errorf("registry: token for %s: %s (anonymous: this node has no login for %s, herdlet -registry-auth)", ref, resp.Status, ref.Registry)
		}
		return "", fmt.Errorf("registry: token for %s: %s (with %s for %s)", ref, resp.Status, whose, ref.Registry)
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

func (c *Client) retries() int {
	switch {
	case c.Retries < 0:
		return 0
	case c.Retries == 0:
		return DefaultRetries
	}
	return c.Retries
}

func (c *Client) maxWait() time.Duration {
	if c.MaxWait > 0 {
		return c.MaxWait
	}
	return DefaultMaxWait
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter reads a Retry-After header: delay-seconds or an HTTP date (RFC 9110 §10.2.3). A date in the past is
// a wait of zero.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(n) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(t.Sub(now), 0), true
}
