package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Policy grants workloads atespace roles (ADR 0001 A-60). herd's own identities need no rule; their grants are built
// in (Builtin): a node (/herd/node/<worker>) is a herdlet that reads every atespace's egress policies, an operator (/herd/operator/<name>) holds operator in every
// atespace, and a node's herd-egress (/herd/egress/<worker>) reads every atespace's egress policies and nothing else
// (RoleEgress: it is no herdlet). The file is JSON:
//
//	{"rules": [
//	  {"id": "spiffe://td/everdict/ws/acme/runner/r1", "spaces": {"acme": "write"}},
//	  {"prefix": "spiffe://td/everdict/ws/acme/", "spaces": {"acme": "read"}},
//	  {"id": "spiffe://td/everdict/control-plane", "spaces": {"*": "read"}}
//	]}
//
// A rule names one SPIFFE ID (id) or every ID under a path prefix (prefix, ending in "/"); an exact id rule wins over
// a prefix, and the longest prefix over a shorter one. spaces maps an atespace (or "*") to read, write or operator.
// A rule may not name an ID under /herd/: those are the built-in conventions'.
type Policy struct {
	exact  map[string]Rule
	prefix []prefixRule
}

// Rule is what a matched SPIFFE ID may do.
type Rule struct {
	Spaces  map[string]Role
	Herdlet bool
}

type prefixRule struct {
	prefix string
	rule   Rule
}

type policyFile struct {
	Rules []struct {
		ID     string          `json:"id"`
		Prefix string          `json:"prefix"`
		Spaces map[string]Role `json:"spaces"`
	} `json:"rules"`
}

// ParsePolicy reads a policy for trust domain td, refusing a rule it could not mean: no id or prefix (or both), an id
// ParseID refuses, another trust domain, a prefix without its trailing "/" (which would let /runner/r1 cover
// /runner/r10), an unknown role, or an ID under /herd/ (built in).
func ParsePolicy(td spiffeid.TrustDomain, b []byte) (*Policy, error) {
	var f policyFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p := &Policy{exact: map[string]Rule{}}
	herd := "spiffe://" + td.Name() + "/herd/"
	for i, r := range f.Rules {
		at := fmt.Sprintf("policy rule %d", i)
		for space, role := range r.Spaces {
			if role != RoleRead && role != RoleWrite && role != RoleOperator {
				return nil, fmt.Errorf("%s: space %q: role %q, want read, write or operator", at, space, role)
			}
		}
		rule := Rule{Spaces: r.Spaces}
		switch {
		case (r.ID == "") == (r.Prefix == ""):
			return nil, fmt.Errorf("%s: name exactly one of id and prefix", at)
		case r.ID != "":
			id, err := spiffeid.FromString(r.ID)
			if err != nil {
				return nil, fmt.Errorf("%s: id: %w", at, err)
			}
			if id.TrustDomain() != td {
				return nil, fmt.Errorf("%s: %s is not in trust domain %q", at, id, td)
			}
			if _, err := ParseID(id); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			if strings.HasPrefix(r.ID, herd) {
				return nil, fmt.Errorf("%s: %s is herd's own identity, whose grant is built in (nodes, operators, egress)", at, r.ID)
			}
			if _, dup := p.exact[r.ID]; dup {
				return nil, fmt.Errorf("%s: %s is named twice", at, r.ID)
			}
			p.exact[r.ID] = rule
		default:
			if !strings.HasPrefix(r.Prefix, "spiffe://"+td.Name()+"/") {
				return nil, fmt.Errorf("%s: prefix %q is not in trust domain %q", at, r.Prefix, td)
			}
			if !strings.HasSuffix(r.Prefix, "/") {
				return nil, fmt.Errorf("%s: prefix %q must end with /", at, r.Prefix)
			}
			if strings.HasPrefix(r.Prefix, herd) || strings.HasPrefix(herd, r.Prefix) {
				return nil, fmt.Errorf("%s: prefix %q covers herd's own identities, whose grants are built in", at, r.Prefix)
			}
			p.prefix = append(p.prefix, prefixRule{r.Prefix, rule})
		}
	}
	return p, nil
}

// Builtin is the grant herd's own identities hold without a policy rule; false for every other kind. herd-api and
// herd-router are no callers of the Control API (the router carries its client's bearer), and neither is an actor yet
// (W-7), so they hold none.
func Builtin(ident Identity) (Rule, bool) {
	switch ident.Kind {
	case KindNode:
		// A node links as its herdlet; a Mac's herd-egress runs in the herdlet's process, so the node also reads the
		// egress policies of every atespace, and nothing else of them.
		return Rule{Herdlet: true, Spaces: map[string]Role{AllSpaces: RoleEgress}}, true
	case KindOperator:
		return Rule{Spaces: map[string]Role{AllSpaces: RoleOperator}}, true
	case KindEgress:
		// herd-egress reads egress policies: it is no herdlet, and reads no actor, template or tag.
		return Rule{Spaces: map[string]Role{AllSpaces: RoleEgress}}, true
	}
	return Rule{}, false
}

// Match is the rule for id: its exact rule, else its longest prefix rule. A nil Policy has none.
func (p *Policy) Match(id spiffeid.ID) (Rule, bool) {
	if p == nil {
		return Rule{}, false
	}
	s := id.String()
	if r, ok := p.exact[s]; ok {
		return r, true
	}
	best, found := -1, false
	var out Rule
	for _, r := range p.prefix {
		if strings.HasPrefix(s, r.prefix) && len(r.prefix) > best {
			best, out, found = len(r.prefix), r.rule, true
		}
	}
	return out, found
}
