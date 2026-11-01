package registry

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

const (
	user, password = "everdict-bot", "ghp_s3cret-pull-token"
)

// private is a two-platform image under team/tool:1 on a registry that keeps it behind a login (imagetest.Private).
func private(t *testing.T, basic bool) (*imagetest.Private, image.Ref) {
	t.Helper()
	s := imagetest.NewServer()
	var ms []imagetest.Manifest
	for _, arch := range []string{"amd64", "arm64"} {
		ms = append(ms, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: arch},
			Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "arch", Body: arch})}})
	}
	s.Push("team/tool", "1", ms...)
	p := &imagetest.Private{Registry: s, User: user, Password: password, IdentityToken: "identity-" + password, Basic: basic}
	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	ref, err := image.ParseRef(strings.TrimPrefix(ts.URL, "http://") + "/team/tool:1")
	if err != nil {
		t.Fatal(err)
	}
	return p, ref
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func authConfig(host, u, p string) string {
	return `{"auths": {"` + host + `": {"auth": "` + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) + `"}}}`
}

// noSecret fails if s carries the password in any form the file holds it in.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{password, base64.StdEncoding.EncodeToString([]byte(user + ":" + password))} {
		if strings.Contains(s, secret) {
			t.Errorf("%s carries the secret: %s", what, s)
		}
	}
}

var arm = image.Platform{OS: "linux", Architecture: "arm64"}

// TestPrivatePullNeedsTheNodeLogin (criterion private-pull): a registry whose Bearer realm issues tokens only to a
// login, and answers an anonymous token request 403 as ghcr.io does for a private image, refuses a pull without the
// node's credentials file (no -registry-auth, or the file absent), and serves it once the file holds the login: the
// realm is sent Basic user:password, the registry only the token. The file is read per pull, so one written after
// the client was made is used. Today the pull is anonymous and answers 403 Forbidden.
func TestPrivatePullNeedsTheNodeLogin(t *testing.T) {
	p, ref := private(t, false)
	if _, err := (&Client{}).Resolve(context.Background(), ref, arm); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("anonymous pull of a private image: err = %v, want the realm's 403", err)
	}
	path := filepath.Join(t.TempDir(), "registry-auth.json")
	c := &Client{Credentials: DockerConfig{Path: path}}
	if _, err := c.Resolve(context.Background(), ref, arm); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("pull with the credentials file absent: err = %v, want the realm's 403 (anonymous)", err)
	}
	writeConfig(t, path, authConfig(ref.Registry, user, "wrong"))
	_, err := c.Resolve(context.Background(), ref, arm)
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("pull with a wrong password: err = %v, want the realm's 401", err)
	}
	noSecret(t, "the refused pull's error", err.Error())

	writeConfig(t, path, authConfig(ref.Registry, user, password))
	cache := &image.Cache{Dir: t.TempDir(), Source: c, Builder: &imagetest.Builder{}, Platform: arm}
	r, err := cache.Get(context.Background(), ref.String())
	if err != nil {
		t.Fatalf("pull with the node's login: %v", err)
	}
	if b, _ := os.ReadFile(r.Path); string(b) != "-0644 0:0 5 arch\n" {
		t.Fatalf("rootfs holds %q", b)
	}
	got := p.Requests()
	if got.Tokens == 0 || got.LoginsAtRegistry != 0 {
		t.Errorf("registry saw %+v: want tokens issued to the login, and the login itself sent only to the realm", got)
	}
}

// TestBasicOnlyRegistry (criterion private-pull): a registry with no token service, which challenges for Basic
// credentials, is refused without a login and served with the file's. Today a Basic challenge is unsupported.
func TestBasicOnlyRegistry(t *testing.T) {
	_, ref := private(t, true)
	path := filepath.Join(t.TempDir(), "config.json")
	c := &Client{Credentials: DockerConfig{Path: path}}
	if _, err := c.Resolve(context.Background(), ref, arm); err == nil || !strings.Contains(err.Error(), "Basic") {
		t.Fatalf("Basic-only registry without a login: err = %v", err)
	}
	writeConfig(t, path, authConfig("http://"+ref.Registry+"/v2/", user, password))
	if _, err := c.Resolve(context.Background(), ref, arm); err != nil {
		t.Fatalf("Basic-only registry with the login: %v", err)
	}
}

// TestIdentityTokenLogin (criterion private-pull): a docker config entry with an identitytoken (docker login to a
// registry with an OAuth2 token service) is sent to the realm as a refresh token, and the pull is served.
func TestIdentityTokenLogin(t *testing.T) {
	p, ref := private(t, false)
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"auths": {"`+ref.Registry+`": {"auth": "`+base64.StdEncoding.EncodeToString([]byte("<token>:"))+`", "identitytoken": "identity-`+password+`"}}}`)
	if _, err := (&Client{Credentials: DockerConfig{Path: path}}).Resolve(context.Background(), ref, arm); err != nil {
		t.Fatalf("pull with an identity token: %v", err)
	}
	if got := p.Requests(); got.Tokens == 0 || got.AnonymousTokens != 0 {
		t.Errorf("registry saw %+v", got)
	}
}

// TestLoginNotSentToAPlainRealm: a challenge whose realm is plain HTTP on a host PlainHTTP does not allow gets no
// login (a registry cannot redirect the node's password to a cleartext endpoint).
func TestLoginNotSentToAPlainRealm(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://tokens.example/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	ref, _ := image.ParseRef(host + "/team/tool:1")
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, authConfig(host, user, password))
	c := &Client{PlainHTTP: func(h string) bool { return h == host }, Credentials: DockerConfig{Path: path}}
	_, err := c.Resolve(context.Background(), ref, arm)
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("err = %v, want the login withheld from a plain realm", err)
	}
	noSecret(t, "the error", err.Error())
}

// TestDockerConfigForms: the keys docker login writes (a bare host, a URL, Docker Hub's index URL) name their
// hosts; username and password fields serve as auth does; a missing or empty file has no logins; a file that is not a
// docker config, or a bad auth, is an error that quotes no secret; and a Credential prints redacted.
func TestDockerConfigForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, `{"auths": {
		"https://ghcr.io": {"auth": "`+base64.StdEncoding.EncodeToString([]byte(user+":"+password))+`"},
		"https://index.docker.io/v1/": {"username": "hub", "password": "`+password+`"},
		"registry.example:5000/v2/": {"identitytoken": "idt"}}}`)
	d := DockerConfig{Path: path}
	for host, want := range map[string]Credential{
		"ghcr.io":               {Username: user, Password: password},
		"GHCR.io":               {Username: user, Password: password},
		image.DefaultRegistry:   {Username: "hub", Password: password},
		"registry.example:5000": {IdentityToken: "idt"},
	} {
		c, ok, err := d.Lookup(host)
		if err != nil || !ok || c != want {
			t.Errorf("Lookup(%q) = %v, %v, %v", host, c, ok, err)
		}
	}
	if _, ok, err := d.Lookup("quay.io"); ok || err != nil {
		t.Errorf("Lookup(quay.io) = %v, %v; want none", ok, err)
	}
	if hosts, err := d.Hosts(); err != nil || strings.Join(hosts, ",") != "ghcr.io,registry-1.docker.io,registry.example:5000" {
		t.Errorf("Hosts = %v, %v", hosts, err)
	}
	c, _, _ := d.Lookup("ghcr.io")
	for _, s := range []string{c.String(), fmtV(c)} {
		noSecret(t, "a printed Credential", s)
	}

	for _, body := range []string{"", "  \n"} {
		writeConfig(t, path, body)
		if _, ok, err := d.Lookup("ghcr.io"); ok || err != nil {
			t.Errorf("empty file %q: %v, %v; want no logins", body, ok, err)
		}
	}
	if _, ok, err := (DockerConfig{Path: filepath.Join(dir, "absent.json")}).Lookup("ghcr.io"); ok || err != nil {
		t.Errorf("absent file: %v, %v; want no logins", ok, err)
	}
	for _, body := range []string{
		`{"auths": {"ghcr.io": {"auth": "` + password + `"`,
		`{"auths": {"ghcr.io": {"auth": "!!` + password + `"}}}`,
		`{"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte(password)) + `"}}}`,
		`{"auths": {"ghcr.io": {"username": "` + user + `"}}}`,
	} {
		writeConfig(t, path, body)
		_, _, err := d.Lookup("ghcr.io")
		if err == nil {
			t.Errorf("%s: no error", body)
			continue
		}
		noSecret(t, "the parse error", err.Error())
	}
}

func fmtV(c Credential) string {
	var b strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		b.WriteString(fmt.Sprintf(f, c))
	}
	return b.String()
}
