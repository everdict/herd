package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func TestStaticKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := auth.StaticKey{Key: key, Audience: "herd", Now: func() time.Time { return now }}
	ok, err := auth.Sign(key, auth.Claims{Subject: "everdict", Audience: "herd", ExpiresAt: now.Add(time.Hour).Unix(), Spaces: map[string]auth.Role{"s1": auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Authenticate(context.Background(), ok)
	if err != nil || p.Subject != "everdict" || p.Spaces["s1"] != auth.RoleWrite {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}
	expired, _ := auth.Sign(key, auth.Claims{Subject: "x", Audience: "herd", ExpiresAt: now.Unix()})
	otherKey, _ := auth.Sign([]byte("ffffffffffffffffffffffffffffffff"), auth.Claims{Subject: "x", Audience: "herd", ExpiresAt: now.Add(time.Hour).Unix()})
	wrongAud, _ := auth.Sign(key, auth.Claims{Subject: "x", Audience: "other", ExpiresAt: now.Add(time.Hour).Unix()})
	parts := strings.Split(ok, ".")
	noneAlg := "eyJhbGciOiJub25lIn0." + parts[1] + "."
	for name, tok := range map[string]string{"expired": expired, "other key": otherKey, "audience": wrongAud, "alg none": noneAlg, "garbage": "x.y", "empty": ""} {
		if _, err := a.Authenticate(context.Background(), tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestSpaceRoles(t *testing.T) {
	var z auth.SpaceRoles
	p := auth.Principal{Subject: "u", Spaces: map[string]auth.Role{"w": auth.RoleWrite, "r": auth.RoleRead}}
	cases := []struct {
		space string
		a     auth.Action
		ok    bool
	}{
		{"w", auth.ActionWrite, true}, {"w", auth.ActionRead, true},
		{"r", auth.ActionRead, true}, {"r", auth.ActionWrite, false},
		{"other", auth.ActionRead, false}, {"", auth.ActionRead, false},
	}
	for _, c := range cases {
		err := z.Authorize(p, c.space, c.a)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, auth.ErrPermissionDenied)) {
			t.Errorf("Authorize(%q, %v) = %v, want ok=%v", c.space, c.a, err, c.ok)
		}
	}
	all := auth.Principal{Subject: "admin", Spaces: map[string]auth.Role{auth.AllSpaces: auth.RoleWrite}}
	if err := z.Authorize(all, "any", auth.ActionWrite); err != nil {
		t.Errorf("all spaces: %v", err)
	}
	herdlet := auth.Principal{Subject: "node", Herdlet: true, Spaces: map[string]auth.Role{auth.AllSpaces: auth.RoleHerdlet}}
	if err := z.Authorize(herdlet, "s", auth.ActionRead); err == nil {
		t.Errorf("a herdlet credential was granted Control access")
	}
}
