package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Chain is herd's authenticator since PLT-489 (ADR 0001 A-60): SPIFFE SVIDs, with HS256 shared-key bearers only as a
// dated break-glass. A bearer whose header names HS256 goes to BreakGlass, and only before BreakGlassUntil; every
// call it authenticates is counted by subject (MetricBreakGlassCalls) and logged, rate-limited (Meter.Note). Any other
// bearer is a JWT-SVID, and a call with no bearer is its peer's X.509-SVID; both go to SVID.
type Chain struct {
	// SVID authenticates SVIDs; nil refuses them.
	SVID Authenticator
	// BreakGlass authenticates HS256 bearers until BreakGlassUntil; nil refuses them.
	BreakGlass Authenticator
	// BreakGlassUntil is when the HS256 allowance ends (a bearer is refused from then on); zero with BreakGlass set
	// means no end, which only tests use: herd-api and herd-router refuse to start without a date
	// (-break-glass-hs256-until).
	BreakGlassUntil time.Time
	// Meter counts the break-glass calls (nil: not counted).
	Meter *Meter
	// Now is the clock BreakGlassUntil is read against (default time.Now).
	Now func() time.Time
}

func (c Chain) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token != "" && headerAlg(token) == "HS256" {
		if c.BreakGlass == nil {
			return Principal{}, refuse("HS256 bearers are refused: no break-glass allowance is configured (-break-glass-hs256-until)")
		}
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		if !c.BreakGlassUntil.IsZero() && !now().Before(c.BreakGlassUntil) {
			return Principal{}, refuse("HS256 bearers are refused: the break-glass allowance ended at %s", c.BreakGlassUntil.UTC().Format(time.RFC3339))
		}
		p, err := c.BreakGlass.Authenticate(ctx, token)
		if err != nil {
			return Principal{}, err
		}
		c.Meter.Inc(MetricBreakGlassCalls, "subject", p.Subject)
		c.Meter.Note("break-glass "+p.Subject,
			fmt.Sprintf("auth: break-glass HS256 call by subject %q (ADR 0001 A-60); its later calls are summed once a minute", p.Subject),
			func(n int, d time.Duration) string {
				return fmt.Sprintf("auth: %d break-glass HS256 calls by subject %q in the last %s", n, p.Subject, Seconds(d))
			})
		return p, nil
	}
	if c.SVID == nil {
		if token == "" {
			return Principal{}, refuse("no bearer token")
		}
		return Principal{}, refuse("only HS256 bearers are accepted: no SPIFFE trust bundle is configured")
	}
	return c.SVID.Authenticate(ctx, token)
}

// headerAlg is a JWT's header alg, "" if token is not a JWT.
func headerAlg(token string) string {
	h, _, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if decodePart(h, &hdr) != nil {
		return ""
	}
	return hdr.Alg
}

// Allowance is why a write on an actor was allowed (ActorWriter).
type Allowance string

const (
	// AllowCreator: the caller created the actor.
	AllowCreator Allowance = "creator"
	// AllowOperator: an operator acts on an actor another caller created.
	AllowOperator Allowance = "operator"
	// AllowUnownedOperator: an operator acts on an actor with no creator recorded (created before PLT-489). Counted.
	AllowUnownedOperator Allowance = "unowned-operator"
	// AllowUnownedBreakGlass: an HS256 bearer acts on an actor with no creator recorded. Counted.
	AllowUnownedBreakGlass Allowance = "unowned-break-glass"
)

// Counted reports whether the allowance is one herd counts (MetricUnownedActorWrites).
func (a Allowance) Counted() bool { return a == AllowUnownedOperator || a == AllowUnownedBreakGlass }

// ActorWriter decides a write on an existing actor of space whose status.creator is creator (workload identity spec
// W4): allowed to its creator or to a caller holding RoleOperator in the space; a write role on the space does not
// suffice. An actor with no creator is writable only by an operator or by break-glass (an HS256 bearer), each under
// an allowance the caller counts. The caller has already authorized the write in the space.
func ActorWriter(p Principal, space, creator string) (Allowance, error) {
	operator := p.Spaces[space] == RoleOperator || p.Spaces[AllSpaces] == RoleOperator
	switch {
	case creator != "" && creator == p.Creator():
		return AllowCreator, nil
	case operator && creator == "":
		return AllowUnownedOperator, nil
	case operator:
		return AllowOperator, nil
	case creator == "" && p.Scheme == SchemeHS256:
		return AllowUnownedBreakGlass, nil
	case creator == "":
		return "", fmt.Errorf("%w: %s may not write an actor with no creator recorded (only an operator or break-glass may)", ErrPermissionDenied, p.Creator())
	}
	return "", fmt.Errorf("%w: %s may not write an actor created by %s", ErrPermissionDenied, p.Creator(), creator)
}
