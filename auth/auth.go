// Package auth is herd-api's authentication (who is calling) and per-space authorization (may they do this in that
// atespace). Both are interfaces; this round ships a static-key implementation: HS256 JWTs signed with one shared
// key, carrying the caller's spaces in a claim. An issuer-backed (JWKS) authenticator can replace it later without
// touching the API server.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role is what a principal may do in a space.
type Role string

const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	// RoleHerdlet may connect as a herdlet; it grants nothing on the Control API.
	RoleHerdlet Role = "herdlet"
)

// AllSpaces in a principal's Spaces grants its role in every space.
const AllSpaces = "*"

// Action is what a call does.
type Action int

const (
	ActionRead Action = iota
	ActionWrite
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	// Spaces maps an atespace (or AllSpaces) to the caller's role there.
	Spaces map[string]Role
	// Herdlet is set for a herdlet's credential.
	Herdlet bool
	// Join is set for a join token: it may only be exchanged for a herdlet credential (package join).
	Join bool
}

// Authenticator turns a bearer token into a principal; ErrUnauthenticated otherwise.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// Authorizer decides whether p may do a in space; ErrPermissionDenied otherwise.
type Authorizer interface {
	Authorize(p Principal, space string, a Action) error
}

var (
	ErrUnauthenticated  = errors.New("auth: unauthenticated")
	ErrPermissionDenied = errors.New("auth: permission denied")
)

// SpaceRoles is the per-space Authorizer: the role named for the space (or for AllSpaces) must allow the action.
type SpaceRoles struct{}

func (SpaceRoles) Authorize(p Principal, space string, a Action) error {
	if space == "" {
		return fmt.Errorf("%w: no atespace named", ErrPermissionDenied)
	}
	for _, r := range []Role{p.Spaces[space], p.Spaces[AllSpaces]} {
		if r == RoleWrite || (r == RoleRead && a == ActionRead) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q may not %s in atespace %q", ErrPermissionDenied, p.Subject, actionName(a), space)
}

func actionName(a Action) string {
	if a == ActionWrite {
		return "write"
	}
	return "read"
}

// Claims are the JWT claims herd reads.
type Claims struct {
	Subject   string          `json:"sub"`
	Audience  string          `json:"aud,omitempty"`
	ExpiresAt int64           `json:"exp"`
	NotBefore int64           `json:"nbf,omitempty"`
	Spaces    map[string]Role `json:"herd_spaces,omitempty"`
	Herdlet   bool            `json:"herd_herdlet,omitempty"`
	Join      bool            `json:"herd_join,omitempty"`
}

// StaticKey authenticates HS256 JWTs signed with one key. Tokens must carry sub and exp; aud must equal Audience when
// it is set.
type StaticKey struct {
	Key      []byte
	Audience string
	Now      func() time.Time
}

var b64 = base64.RawURLEncoding

// Sign mints a token for c with key. It exists for operators and tests; herd-api itself never mints.
func Sign(key []byte, c Claims) (string, error) {
	if len(key) < 32 {
		return "", fmt.Errorf("auth: signing key must be at least 32 bytes")
	}
	hdr := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auth: %w", err)
	}
	signing := hdr + "." + b64.EncodeToString(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signing))
	return signing + "." + b64.EncodeToString(m.Sum(nil)), nil
}

func (s StaticKey) Authenticate(ctx context.Context, token string) (Principal, error) {
	if len(s.Key) < 32 {
		return Principal{}, fmt.Errorf("%w: server has no usable key", ErrUnauthenticated)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, fmt.Errorf("%w: not a JWT", ErrUnauthenticated)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := decodePart(parts[0], &hdr); err != nil || hdr.Alg != "HS256" {
		return Principal{}, fmt.Errorf("%w: header must name alg HS256", ErrUnauthenticated)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Principal{}, fmt.Errorf("%w: signature encoding", ErrUnauthenticated)
	}
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return Principal{}, fmt.Errorf("%w: bad signature", ErrUnauthenticated)
	}
	var c Claims
	if err := decodePart(parts[1], &c); err != nil {
		return Principal{}, fmt.Errorf("%w: claims: %v", ErrUnauthenticated, err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().Unix()
	switch {
	case c.Subject == "":
		return Principal{}, fmt.Errorf("%w: no sub", ErrUnauthenticated)
	case c.ExpiresAt == 0 || t >= c.ExpiresAt:
		return Principal{}, fmt.Errorf("%w: expired or no exp", ErrUnauthenticated)
	case c.NotBefore != 0 && t < c.NotBefore:
		return Principal{}, fmt.Errorf("%w: not yet valid", ErrUnauthenticated)
	case s.Audience != "" && c.Audience != s.Audience:
		return Principal{}, fmt.Errorf("%w: audience %q", ErrUnauthenticated, c.Audience)
	}
	if c.Join && (c.Herdlet || len(c.Spaces) > 0) {
		return Principal{}, fmt.Errorf("%w: a join token grants nothing else", ErrUnauthenticated)
	}
	return Principal{Subject: c.Subject, Spaces: c.Spaces, Herdlet: c.Herdlet, Join: c.Join}, nil
}

func decodePart(p string, v any) error {
	b, err := b64.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Bearer is a client-side per-call credential carrying a token as "authorization: Bearer <token>" (it satisfies
// grpc's credentials.PerRPCCredentials). It does not require transport security: herd-api serves cleartext HTTP/2 and
// TLS is terminated in front of it (ADR 0001, A-12).
type Bearer string

// GetRequestMetadata returns the authorization metadata.
func (b Bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

// RequireTransportSecurity reports false; see Bearer.
func (Bearer) RequireTransportSecurity() bool { return false }
