package egress

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNoCredential is returned when a credential reference resolves to nothing.
var ErrNoCredential = errors.New("egress: no such credential")

// Credentials resolves a credential_uri (ate-secret://<provider-class>/<provider-name>/<tail>) to the secret value.
// It runs in herd-egress, on the host: the actor never sees what it returns.
type Credentials interface {
	Resolve(ctx context.Context, actor Actor, uri string) (string, error)
}

// Provider serves the credentials of one provider class.
type Provider interface {
	Credential(ctx context.Context, actor Actor, name, tail string) (string, error)
}

// Providers dispatches a credential_uri to the provider registered for its class.
type Providers map[string]Provider

// Resolve implements Credentials.
func (p Providers) Resolve(ctx context.Context, actor Actor, uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, "ate-secret://")
	if !ok {
		return "", fmt.Errorf("%w: %q is not ate-secret://", ErrNoCredential, uri)
	}
	class, rest, _ := strings.Cut(rest, "/")
	name, tail, _ := strings.Cut(rest, "/")
	prov, ok := p[class]
	if !ok || name == "" {
		return "", fmt.Errorf("%w: no provider class %q", ErrNoCredential, class)
	}
	return prov.Credential(ctx, actor, name, tail)
}

// StaticProvider holds credentials in memory, keyed "<provider-name>/<tail>".
type StaticProvider map[string]string

// Credential implements Provider.
func (s StaticProvider) Credential(_ context.Context, _ Actor, name, tail string) (string, error) {
	v, ok := s[name+"/"+tail]
	if !ok {
		return "", fmt.Errorf("%w: %s/%s", ErrNoCredential, name, tail)
	}
	return v, nil
}

// DirProvider reads <Root>/<provider-name>/<tail> on the host, without following a path out of Root. Trailing
// newlines are dropped.
type DirProvider struct {
	Root string
}

// Credential implements Provider.
func (d DirProvider) Credential(_ context.Context, _ Actor, name, tail string) (string, error) {
	r, err := os.OpenRoot(d.Root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := r.ReadFile(name + "/" + tail)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoCredential, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// ActorJWTs mints the JWT an actor_jwt header carries: it asserts which actor sent the request.
type ActorJWTs interface {
	ActorJWT(ctx context.Context, actor Actor, audiences []string, lifetime time.Duration) (string, error)
}

// ES256Minter signs actor JWTs with an EC P-256 key; relying parties verify them against Issuer's keys. A JWT is
// reused while more than half of its lifetime remains.
type ES256Minter struct {
	Issuer string
	Key    *ecdsa.PrivateKey
	// KeyID is the "kid" header; empty for none.
	KeyID string
	Now   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedJWT
}

type cachedJWT struct {
	token string
	exp   time.Time
	half  time.Time
}

// ActorJWT implements ActorJWTs.
func (m *ES256Minter) ActorJWT(_ context.Context, actor Actor, audiences []string, lifetime time.Duration) (string, error) {
	if m.Key == nil {
		return "", errors.New("egress: no JWT signing key")
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	ck := actor.String() + "\x00" + strings.Join(audiences, "\x00") + "\x00" + lifetime.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[ck]; ok && now.Before(c.half) {
		return c.token, nil
	}
	exp := now.Add(lifetime)
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	claims := map[string]any{
		"iss": m.Issuer, "sub": actor.String(), "aud": audiences,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": exp.Unix(),
		"jti":      base64.RawURLEncoding.EncodeToString(jti),
		"atespace": actor.Space, "actor": actor.Name, "actor_uid": actor.UID,
	}
	hdr := map[string]string{"alg": "ES256", "typ": "JWT"}
	if m.KeyID != "" {
		hdr["kid"] = m.KeyID
	}
	h, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, m.Key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	tok := signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	if m.cache == nil {
		m.cache = map[string]cachedJWT{}
	}
	m.cache[ck] = cachedJWT{token: tok, exp: exp, half: now.Add(lifetime / 2)}
	return tok, nil
}

// VerifyES256 checks a token's signature against pub and returns its claims (used by tests and relying parties that
// trust this minter directly).
func VerifyES256(tok string, pub *ecdsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: not three parts")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("jwt: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("jwt: bad signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	return claims, json.Unmarshal(b, &claims)
}
