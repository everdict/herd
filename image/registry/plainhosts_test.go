package registry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// TestPlainHosts (criterion insecure-registry): PlainHosts speaks plain HTTP to the loopback hosts and to exactly the
// named host[:port]s, as the docker daemon's insecure-registries; every other host, the same address on another port
// or without one included, stays https.
func TestPlainHosts(t *testing.T) {
	plain := PlainHosts([]string{"100.69.164.81:5001", "Registry.Lab"})
	for host, want := range map[string]bool{
		"100.69.164.81:5001": true,
		"registry.lab":       true,
		"REGISTRY.LAB":       true,
		"localhost:5000":     true,
		"127.0.0.1:5000":     true,
		"100.69.164.81":      false,
		"100.69.164.81:5002": false,
		"100.69.164.82:5001": false,
		"registry.lab:5000":  false,
		"ghcr.io":            false,
	} {
		if got := plain(host); got != want {
			t.Errorf("plain(%q) = %v, want %v", host, got, want)
		}
	}
	var c Client
	if got := c.base(context.Background(), "100.69.164.81:5001"); got != "https://100.69.164.81:5001" {
		t.Errorf("default client: %s, want https for a non-loopback host", got)
	}
	c.PlainHTTP = plain
	if got := c.base(context.Background(), "100.69.164.81:5001"); got != "http://100.69.164.81:5001" {
		t.Errorf("with PlainHosts: %s, want plain HTTP to the named host", got)
	}
}

// TestPullPlainHosts (DEFAUL-1600, criterion a-pull-names-its-plain-hosts): a pull's own plain hosts (WithPlainHosts,
// a template's image_plain_http_hosts) make a non-loopback store and its token realm on another host plain HTTP for
// that pull, with no PlainHTTP on the Client (no -insecure-registry); the same Client's pulls without them stay https.
// The credential rule holds: with only the store named, the login is not sent to its plain realm.
func TestPullPlainHosts(t *testing.T) {
	const storeHost, realmHost = "store.cluster.test:5001", "realm.cluster.test:8787"
	s := imagetest.NewServer()
	s.Push("team/tool", "1", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "arch", Body: "amd64"})}})
	p := &imagetest.Private{Registry: s, User: user, Password: password, Realm: realmHost}
	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	dial := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ts.Listener.Addr().String())
	}}
	logins, err := ParseLogins([]byte(authConfig(storeHost, user, password)), "image_pull_auth")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: &http.Client{Transport: dial}}
	ref := image.Ref{Registry: storeHost, Repository: "team/tool", Tag: "1"}
	amd := image.Platform{OS: "linux", Architecture: "amd64"}
	pull := func(plain ...string) error {
		ctx := WithLogins(context.Background(), logins, "the template's image_pull_auth")
		if plain != nil {
			ctx = WithPlainHosts(ctx, plain)
		}
		_, err := c.Resolve(ctx, ref, amd)
		return err
	}

	if err := pull(); err == nil || !strings.Contains(err.Error(), "https://"+storeHost) {
		t.Errorf("pull naming no plain host: %v, want https to %s", err, storeHost)
	}
	if err := pull(storeHost); err == nil || !strings.Contains(err.Error(), "realm http://"+realmHost+"/token is not https") {
		t.Errorf("pull naming the store, not its realm: %v, want the login kept from the plain realm", err)
	}
	if r := p.Requests(); r.Tokens+r.BadLogins+r.AnonymousTokens != 0 {
		t.Errorf("the realm was asked for a token (%+v) before a pull named it plain", r)
	}
	if err := pull(storeHost, realmHost); err != nil {
		t.Fatalf("pull naming the store and its realm: %v", err)
	}
	if p.Requests().Tokens != 1 {
		t.Errorf("realm requests %+v, want one token issued to the login", p.Requests())
	}
	if err := pull(); err == nil || !strings.Contains(err.Error(), "https://"+storeHost) {
		t.Errorf("a later pull naming no plain host: %v, want https again", err)
	}
	for _, h := range []string{"", "http://" + storeHost, storeHost + "/v2", "u@" + storeHost, ":5001"} {
		if CheckHost(h) == nil {
			t.Errorf("CheckHost(%q) accepted it", h)
		}
	}
	for _, h := range []string{storeHost, "registry.lab", "[::1]:5000"} {
		if err := CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q): %v", h, err)
		}
	}
}
