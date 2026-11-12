package auth_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
)

// TestChainCountsBreakGlass (PLT-489, A-60): HS256 stays accepted beside SPIFFE, as break-glass: every call it
// authenticates is counted by subject on the meter herd-api serves, and logged naming the subject. An SVID call
// counts nothing.
func TestChainCountsBreakGlass(t *testing.T) {
	c := newCA(t, td)
	s, _ := newSPIFFE(t, c.spiffeBundle(t, td))
	m := auth.NewMeter()
	chain := auth.Chain{SVID: s, BreakGlass: auth.StaticKey{Key: key}, Meter: m}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	hs, err := auth.Sign(key, auth.Claims{Subject: "everdict", ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: map[string]auth.Role{"acme": auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		p, err := chain.Authenticate(context.Background(), hs)
		if err != nil || p.Scheme != auth.SchemeHS256 || p.Creator() != "hs256:everdict" {
			t.Fatalf("an HS256 bearer: %+v, %v; want the break-glass principal hs256:everdict", p, err)
		}
	}
	if n := m.Value(auth.MetricBreakGlassCalls, "subject", "everdict"); n != 2 {
		t.Errorf("%s{subject=everdict} = %d after two HS256 calls, want 2", auth.MetricBreakGlassCalls, n)
	}
	if !strings.Contains(logs.String(), `break-glass HS256 call by subject "everdict"`) {
		t.Errorf("log %q does not name the break-glass subject", logs.String())
	}
	if _, err := chain.Authenticate(context.Background(), c.jwtSVID(t, runnerID, []string{audience}, time.Now().Add(time.Minute))); err != nil {
		t.Fatalf("a JWT-SVID through the chain: %v", err)
	}
	if _, err := chain.Authenticate(withChain(c.x509SVID(t, runnerID, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))), ""); err != nil {
		t.Fatalf("an X.509-SVID through the chain: %v", err)
	}
	if n := m.Value(auth.MetricBreakGlassCalls, "subject", "everdict"); n != 2 {
		t.Errorf("SVID calls moved the break-glass count to %d", n)
	}
	var text bytes.Buffer
	if err := m.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if want := auth.MetricBreakGlassCalls + `{subject="everdict"} 2`; !strings.Contains(text.String(), want) {
		t.Errorf("metrics text %q does not carry %q", text.String(), want)
	}

	// A chain without a break-glass key refuses HS256; without SPIFFE it refuses an SVID and a call with neither.
	noKey := auth.Chain{SVID: s, Meter: m}
	_, err = noKey.Authenticate(context.Background(), hs)
	wantRefused(t, "HS256 with no break-glass key", err, "no break-glass allowance")
	keyOnly := auth.Chain{BreakGlass: auth.StaticKey{Key: key}, Meter: m}
	_, err = keyOnly.Authenticate(context.Background(), c.jwtSVID(t, runnerID, []string{audience}, time.Now().Add(time.Minute)))
	wantRefused(t, "a JWT-SVID with no trust bundle", err, "no SPIFFE trust bundle")
	_, err = keyOnly.Authenticate(context.Background(), "")
	wantRefused(t, "no credential", err, "no bearer token")
}

// TestActorWriter (A-60): a write on an actor is its creator's or an operator's; a write role on the space does not
// suffice. An actor with no creator (made before PLT-489) is writable by an operator or by break-glass, each under
// its own allowance, which the caller counts.
func TestActorWriter(t *testing.T) {
	runner := auth.Principal{Subject: runnerID, Scheme: auth.SchemeJWTSVID, Spaces: map[string]auth.Role{"acme": auth.RoleWrite}}
	other := auth.Principal{Subject: "spiffe://herd.test/everdict/ws/acme/runner/r2", Scheme: auth.SchemeX509SVID, Spaces: map[string]auth.Role{"acme": auth.RoleWrite}}
	op := auth.Principal{Subject: "spiffe://herd.test/herd/operator/alice", Scheme: auth.SchemeX509SVID, Spaces: map[string]auth.Role{"*": auth.RoleOperator}}
	glass := auth.Principal{Subject: "everdict", Spaces: map[string]auth.Role{"acme": auth.RoleWrite}}
	for _, tc := range []struct {
		what    string
		p       auth.Principal
		creator string
		want    auth.Allowance
	}{
		{"the creator", runner, runnerID, auth.AllowCreator},
		{"the HS256 creator", glass, "hs256:everdict", auth.AllowCreator},
		{"an operator on another's actor", op, runnerID, auth.AllowOperator},
		{"an operator on an actor with no creator", op, "", auth.AllowUnownedOperator},
		{"break-glass on an actor with no creator", glass, "", auth.AllowUnownedBreakGlass},
		{"another writer of the space", other, runnerID, ""},
		{"break-glass on an SVID's actor", glass, runnerID, ""},
		{"an SVID writer on an actor with no creator", runner, "", ""},
		{"a writer named like the creator but by another scheme", auth.Principal{Subject: runnerID, Spaces: runner.Spaces}, runnerID, ""},
		{"the creator of herd-api's own golden actor", runner, "herd-api", ""},
	} {
		got, err := auth.ActorWriter(tc.p, "acme", tc.creator)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: allowed (%s), want refused", tc.what, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.what, got, err, tc.want)
		}
	}
}

// TestBreakGlassIsDated (A-60): an HS256 bearer is refused unless a break-glass allowance is configured, and from its
// end date on.
func TestBreakGlassIsDated(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	hs, err := auth.Sign(key, auth.Claims{Subject: "everdict", ExpiresAt: now.Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = auth.Chain{}.Authenticate(ctx, hs)
	wantRefused(t, "HS256 with no allowance", err, "no break-glass allowance")
	until := now.Add(time.Hour)
	c := auth.Chain{BreakGlass: auth.StaticKey{Key: key, Now: func() time.Time { return now }}, BreakGlassUntil: until, Now: func() time.Time { return now }}
	if _, err := c.Authenticate(ctx, hs); err != nil {
		t.Fatalf("HS256 before the allowance ends: %v", err)
	}
	c.Now = func() time.Time { return until }
	_, err = c.Authenticate(ctx, hs)
	wantRefused(t, "HS256 at the allowance's end", err, "allowance ended")
}
