package imagetest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Private keeps a registry behind a login, as ghcr.io keeps a private image. By default it is a token registry: a
// request without a token it issued is answered 401 with a Bearer challenge whose realm (/token on the same host)
// issues tokens only to the login (Basic user:password on a GET, or IdentityToken as an OAuth2 refresh token on a
// POST), and answers 403 to an anonymous token request, as ghcr.io does. With Basic set it has no token service and
// challenges for Basic credentials on every request instead.
type Private struct {
	Registry       http.Handler
	User, Password string
	// IdentityToken, if set, is accepted as a refresh_token at the realm (grant_type=refresh_token).
	IdentityToken string
	Basic         bool

	mu     sync.Mutex
	issued map[string]bool
	seen   PrivateRequests
}

// PrivateRequests counts what a Private registry was asked.
type PrivateRequests struct {
	// Tokens were issued; AnonymousTokens and BadLogins were refused at the realm.
	Tokens, AnonymousTokens, BadLogins int
	// Unauthorized registry requests were answered with a challenge.
	Unauthorized int
	// LoginsAtRegistry are registry (/v2/) requests that carried the login itself while the registry wanted a token.
	LoginsAtRegistry int
}

// Requests is what the registry was asked so far.
func (p *Private) Requests() PrivateRequests {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

func (p *Private) login(r *http.Request) bool {
	u, pw, ok := r.BasicAuth()
	return ok && u == p.User && pw == p.Password
}

func (p *Private) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.Basic && r.URL.Path == "/token" {
		p.token(w, r)
		return
	}
	if p.admit(w, r) {
		p.Registry.ServeHTTP(w, r)
	}
}

// admit answers the challenge to a registry request without the login (Basic) or a token it issued, and says
// whether the request may be served.
func (p *Private) admit(w http.ResponseWriter, r *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Basic {
		if p.login(r) {
			return true
		}
		p.seen.Unauthorized++
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		http.Error(w, "login required", http.StatusUnauthorized)
		return false
	}
	if _, _, ok := r.BasicAuth(); ok {
		p.seen.LoginsAtRegistry++
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && p.issued[tok] {
		return true
	}
	p.seen.Unauthorized++
	repo := strings.TrimPrefix(r.URL.Path, "/v2/")
	for _, k := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(repo, k); i >= 0 {
			repo = repo[:i]
		}
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="private",scope="repository:`+repo+`:pull"`)
	http.Error(w, "token required", http.StatusUnauthorized)
	return false
}

func (p *Private) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ok := false
	switch r.Method {
	case http.MethodPost:
		if err := r.ParseForm(); err == nil && r.PostForm.Get("grant_type") == "refresh_token" {
			ok = p.IdentityToken != "" && r.PostForm.Get("refresh_token") == p.IdentityToken
		}
	default:
		if _, _, given := r.BasicAuth(); !given {
			p.seen.AnonymousTokens++
			http.Error(w, "anonymous pull of a private image", http.StatusForbidden)
			return
		}
		ok = p.login(r)
	}
	if !ok {
		p.seen.BadLogins++
		http.Error(w, "bad login", http.StatusUnauthorized)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	if p.issued == nil {
		p.issued = map[string]bool{}
	}
	p.issued[tok] = true
	p.seen.Tokens++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok, "access_token": tok})
}
