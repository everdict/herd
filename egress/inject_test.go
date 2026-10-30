package egress_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/egress"
)

const secret = "sk-live-0123456789abcdef"

// TestHeaderInjection (criterion header-injection): an https (intercepted) rule and an http rule replace the
// Authorization header with a credential from the host's store and X-Actor-Token with an actor JWT. The actor sends
// its own value; the upstream sees only the credential. A request without the header is forwarded without it. The
// secret never crosses the actor's side of the flow, in either direction.
func TestHeaderInjection(t *testing.T) {
	w := newWorld(t)
	w.resolve("api.vendor.example", []string{"198.51.100.40"})
	w.resolve("plain.vendor.example", []string{"198.51.100.41"})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(t, w, func(c *egress.Config, _ *egress.GuardConfig) {
		c.Credentials = egress.Providers{"static": egress.StaticProvider{"vault/openai": secret}}
		c.JWTs = &egress.ES256Minter{Issuer: "https://herd.example", Key: key}
	})
	effects := &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{
		{Header: "authorization", Prefix: "Bearer ", CredentialUri: "ate-secret://static/vault/openai"},
		{Header: "X-Actor-Token", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"https://vendor.example"}, ExpirationSeconds: 600}},
	}}
	bind(t, g, actor1, compile(t,
		httpsRule(effects, "api.vendor.example"),
		&ateapi.EgressRule{Http: &ateapi.HttpRule{Hostnames: []string{"plain.vendor.example"}, Effects: effects}}))

	rec := &recorder{}
	c := g.client(t, actor1, "198.51.100.40")
	c.Transport.(*http.Transport).DialContext = rec.wrap(c.Transport.(*http.Transport).DialContext)

	for _, u := range []string{"https://api.vendor.example/v1/chat", "http://plain.vendor.example/v1/chat"} {
		up := w.httpsUp
		if strings.HasPrefix(u, "http:") {
			up = w.httpUp
		}
		code, body := get(t, c, u, map[string]string{"Authorization": "Bearer actor-placeholder", "X-Actor-Token": "x"})
		if code != 200 {
			t.Fatalf("%s: %d %q", u, code, body)
		}
		r := up.last()
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("%s: upstream Authorization = %q, want the injected credential", u, got)
		}
		if got := r.Header.Values("Authorization"); len(got) != 1 {
			t.Errorf("%s: upstream got %d Authorization values: %q", u, len(got), got)
		}
		claims, err := egress.VerifyES256(r.Header.Get("X-Actor-Token"), &key.PublicKey)
		if err != nil {
			t.Errorf("%s: X-Actor-Token: %v", u, err)
		} else if claims["sub"] != "space-1/a1" || claims["actor_uid"] != "uid-a1" {
			t.Errorf("%s: JWT claims %v", u, claims)
		}

		// Without the header: forwarded unchanged, no credential.
		if code, _ := get(t, c, u, nil); code != 200 {
			t.Fatalf("%s without header: %d", u, code)
		}
		if got := up.last().Header.Get("Authorization"); got != "" {
			t.Errorf("%s: request without Authorization got %q upstream", u, got)
		}
	}
	if rec.contains(secret) {
		t.Errorf("the secret crossed the actor's side of a flow")
	}
	if !rec.contains("actor-placeholder") {
		t.Errorf("recorder saw no actor traffic (cleartext request should carry the placeholder)")
	}
}

// recorder keeps every byte that crosses the actor's end of its flows.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) wrap(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, n, a string) (net.Conn, error) {
		c, err := dial(ctx, n, a)
		if err != nil {
			return nil, err
		}
		return &recConn{Conn: c, r: r}, nil
	}
}

func (r *recorder) contains(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Contains(r.buf.Bytes(), []byte(s))
}

type recConn struct {
	net.Conn
	r *recorder
}

func (c *recConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.r.mu.Lock()
	c.r.buf.Write(p[:n])
	c.r.mu.Unlock()
	return n, err
}

func (c *recConn) Write(p []byte) (int, error) {
	c.r.mu.Lock()
	c.r.buf.Write(p)
	c.r.mu.Unlock()
	return c.Conn.Write(p)
}

func TestInjectionNeedsDecidingRule(t *testing.T) {
	// Effects apply only from the deciding rule: a wildcard https rule with a credential does not inject into a
	// request that an exact rule without effects decides.
	w := newWorld(t)
	w.resolve("exact.vendor.example", []string{"198.51.100.42"})
	g := newGateway(t, w, func(c *egress.Config, _ *egress.GuardConfig) {
		c.Credentials = egress.Providers{"static": egress.StaticProvider{"vault/openai": secret}}
	})
	bind(t, g, actor1, compile(t,
		httpsRule(&ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{{Header: "Authorization", CredentialUri: "ate-secret://static/vault/openai"}}}, "*.vendor.example"),
		httpsRule(nil, "exact.vendor.example")))
	c := g.client(t, actor1, "198.51.100.42")
	if code, body := get(t, c, "https://exact.vendor.example/", map[string]string{"Authorization": "mine"}); code != 200 {
		t.Fatalf("%d %q", code, body)
	}
	if got := w.httpsUp.last().Header.Get("Authorization"); got != "mine" {
		t.Errorf("Authorization = %q, want the actor's own value (exact rule has no effects)", got)
	}
}
