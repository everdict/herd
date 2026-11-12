package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Policy maps SPIFFE IDs to what they may do in herd (ADR 0001 A-60). It is a JSON file:
//
//	{"rules": [
//	  {"id": "spiffe://td/everdict/ws/acme/runner/r1", "spaces": {"acme": "write"}},
//	  {"prefix": "spiffe://td/everdict/ws/acme/", "spaces": {"acme": "read"}},
//	  {"prefix": "spiffe://td/herd/node/", "herdlet": true},
//	  {"id": "spiffe://td/herd/operator/alice", "spaces": {"*": "operator"}}
//	]}
//
// A rule names one SPIFFE ID (id) or every ID under a path prefix (prefix, ending in "/"); an exact id rule wins over
// a prefix, and the longest prefix over a shorter one. spaces maps an atespace (or "*") to read, write or operator;
// herdlet makes a node's credential, and only IDs under /herd/node/ may hold it.
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
		ID      string          `json:"id"`
		Prefix  string          `json:"prefix"`
		Spaces  map[string]Role `json:"spaces"`
		Herdlet bool            `json:"herdlet"`
	} `json:"rules"`
}

// ParsePolicy reads a policy for trust domain td, refusing a rule it could not mean: no id or prefix (or both), an id
// ParseID refuses, another trust domain, a prefix without its trailing "/" (which would let /runner/r1 cover
// /runner/r10), an unknown role, or herdlet outside /herd/node/.
func ParsePolicy(td spiffeid.TrustDomain, b []byte) (*Policy, error) {
	var f policyFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p := &Policy{exact: map[string]Rule{}}
	nodes := "spiffe://" + td.Name() + "/herd/node/"
	for i, r := range f.Rules {
		at := fmt.Sprintf("policy rule %d", i)
		for space, role := range r.Spaces {
			if role != RoleRead && role != RoleWrite && role != RoleOperator {
				return nil, fmt.Errorf("%s: space %q: role %q, want read, write or operator", at, space, role)
			}
		}
		rule := Rule{Spaces: r.Spaces, Herdlet: r.Herdlet}
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
			if r.Herdlet && !strings.HasPrefix(r.ID, nodes) {
				return nil, fmt.Errorf("%s: only an id under %s may be a herdlet", at, nodes)
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
			if r.Herdlet && !strings.HasPrefix(r.Prefix, nodes) {
				return nil, fmt.Errorf("%s: only a prefix under %s may be a herdlet", at, nodes)
			}
			p.prefix = append(p.prefix, prefixRule{r.Prefix, rule})
		}
		if r.Herdlet && len(r.Spaces) > 0 {
			return nil, fmt.Errorf("%s: a herdlet rule grants no spaces", at)
		}
	}
	return p, nil
}

// Match is the rule for id: its exact rule, else its longest prefix rule.
func (p *Policy) Match(id spiffeid.ID) (Rule, bool) {
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
