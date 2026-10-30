// Package policy evaluates Substrate's EgressPolicy (substrate.ateapi.v1.EgressPolicy, pinned in api/ateapi/v1) the
// way herd-egress enforces it: default-deny, rules matched by destination NAME and port, never by address.
//
// A decision is made on one of three inputs, each by the rules that decide on it:
//   - a cleartext HTTP request, on its authority (Host / :authority) and the port the actor connected to: http rules;
//   - a TLS ClientHello, on its SNI and port: https (intercepted) and tls_passthrough rules;
//   - a request inside an intercepted connection, on its authority and port: https rules.
//
// Among the rules that match, the winner is the one whose matching pattern has no wildcard, then the one that names
// the port rather than all ports. Two rules that would tie are refused when the policy is compiled, as Substrate
// refuses them at admission. Only the winner's effects apply.
package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"net/textproto"
	"regexp"
	"strings"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
)

// ErrInvalid wraps every reason a policy does not compile.
var ErrInvalid = errors.New("invalid egress policy")

// Protocol is a rule's protocol.
type Protocol int

const (
	ProtocolNone Protocol = iota
	// ProtocolHTTP is cleartext HTTP (HttpRule).
	ProtocolHTTP
	// ProtocolHTTPS is HTTPS intercepted by the gateway (HttpsRule).
	ProtocolHTTPS
	// ProtocolTLSPassthrough is TLS forwarded without decryption (TlsPassthroughRule).
	ProtocolTLSPassthrough
)

func (p Protocol) String() string {
	switch p {
	case ProtocolHTTP:
		return "http"
	case ProtocolHTTPS:
		return "https"
	case ProtocolTLSPassthrough:
		return "tls_passthrough"
	}
	return "none"
}

// Input is what the gateway has seen when it asks for a decision.
type Input int

const (
	// InputHTTPRequest is a cleartext HTTP request.
	InputHTTPRequest Input = iota + 1
	// InputTLSHello is a TLS ClientHello.
	InputTLSHello
	// InputInterceptedRequest is a request inside a connection the gateway intercepted under an https rule.
	InputInterceptedRequest
)

func (in Input) decidedBy(p Protocol) bool {
	switch in {
	case InputHTTPRequest:
		return p == ProtocolHTTP
	case InputTLSHello:
		return p == ProtocolHTTPS || p == ProtocolTLSPassthrough
	case InputInterceptedRequest:
		return p == ProtocolHTTPS
	}
	return false
}

// Header is one replace_headers effect: when a request carries Name, its value becomes Prefix + the credential.
type Header struct {
	// Name is the canonical header name.
	Name   string
	Prefix string
	// CredentialURI is set for a credential reference (ate-secret://...).
	CredentialURI string
	// JWT is set for a Substrate-style actor JWT.
	JWT *JWTSource
}

// JWTSource asks for a JWT asserting the actor's identity.
type JWTSource struct {
	Audiences         []string
	ExpirationSeconds int64
}

// Rule is one compiled rule.
type Rule struct {
	Protocol Protocol
	// Patterns are hostname patterns: a DNS name, or one with "*" as its leftmost label, or "*" alone.
	Patterns []string
	// Ports lists the ports matched; nil means every port.
	Ports   []int
	Headers []Header
}

// Decision is the outcome for one input.
type Decision struct {
	// Allowed is true when a rule decided; then Rule is the winner.
	Allowed bool
	Rule    *Rule
	// Reason says why traffic was refused.
	Reason string
}

// Policy is a compiled EgressPolicy. The zero Policy (and a nil one) refuses everything.
type Policy struct {
	rules []*Rule
}

// Deny is the policy of an actor that has none: every destination is refused.
var Deny = &Policy{}

// Rules returns the compiled rules.
func (p *Policy) Rules() []*Rule {
	if p == nil {
		return nil
	}
	return p.rules
}

// Compile validates p as Substrate's admission would and compiles it. A nil policy compiles to Deny.
func Compile(p *ateapi.EgressPolicy) (*Policy, error) {
	if p == nil {
		return Deny, nil
	}
	return CompileRules(p.GetRules())
}

// CompileRules compiles a rule list (EgressPolicy.rules or EgressPolicyTemplate.rules).
func CompileRules(in []*ateapi.EgressRule) (*Policy, error) {
	if len(in) > 256 {
		return nil, fmt.Errorf("%w: %d rules, at most 256", ErrInvalid, len(in))
	}
	out := &Policy{}
	for i, r := range in {
		c, err := compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("%w: rules[%d]: %v", ErrInvalid, i, err)
		}
		out.rules = append(out.rules, c)
	}
	if err := out.checkTies(); err != nil {
		return nil, err
	}
	return out, nil
}

func compileRule(r *ateapi.EgressRule) (*Rule, error) {
	set := 0
	var hostnames []string
	var ports *ateapi.Ports
	var effects *ateapi.HttpRuleEffects
	var c Rule
	defaultPort := 0
	if h := r.GetHttp(); h != nil {
		set++
		c.Protocol, hostnames, ports, effects, defaultPort = ProtocolHTTP, h.GetHostnames(), h.GetPorts(), h.GetEffects(), 80
	}
	if h := r.GetHttps(); h != nil {
		set++
		c.Protocol, hostnames, ports, effects, defaultPort = ProtocolHTTPS, h.GetHostnames(), h.GetPorts(), h.GetEffects(), 443
	}
	if t := r.GetTlsPassthrough(); t != nil {
		set++
		c.Protocol, hostnames, ports = ProtocolTLSPassthrough, t.GetHostnames(), t.GetPorts()
		if ports == nil {
			return nil, errors.New("tls_passthrough needs ports")
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("exactly one of http, https, tls_passthrough must be set, got %d", set)
	}
	if len(hostnames) == 0 || len(hostnames) > 256 {
		return nil, fmt.Errorf("%s: 1 to 256 hostnames required, got %d", c.Protocol, len(hostnames))
	}
	seen := map[string]bool{}
	for _, h := range hostnames {
		if err := ValidPattern(h); err != nil {
			return nil, err
		}
		if seen[h] {
			return nil, fmt.Errorf("hostname %q repeated", h)
		}
		seen[h] = true
		c.Patterns = append(c.Patterns, h)
	}
	switch {
	case ports == nil:
		c.Ports = []int{defaultPort}
	case ports.GetAll() != nil && len(ports.GetNumbers()) > 0:
		return nil, errors.New("ports: all and numbers are exclusive")
	case ports.GetAll() != nil:
		c.Ports = nil
	default:
		nums := ports.GetNumbers()
		if len(nums) == 0 || len(nums) > 16 {
			return nil, fmt.Errorf("ports: 1 to 16 numbers required, got %d", len(nums))
		}
		pseen := map[int]bool{}
		for _, n := range nums {
			if n < 1 || n > 65535 {
				return nil, fmt.Errorf("port %d out of range", n)
			}
			if pseen[int(n)] {
				return nil, fmt.Errorf("port %d repeated", n)
			}
			pseen[int(n)] = true
			c.Ports = append(c.Ports, int(n))
		}
	}
	if effects != nil {
		if c.Protocol == ProtocolTLSPassthrough {
			return nil, errors.New("tls_passthrough has no effects")
		}
		hs, err := compileHeaders(effects.GetReplaceHeaders())
		if err != nil {
			return nil, err
		}
		c.Headers = hs
	}
	return &c, nil
}

var headerRE = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

func compileHeaders(in []*ateapi.CredentialHeader) ([]Header, error) {
	if len(in) == 0 || len(in) > 16 {
		return nil, fmt.Errorf("effects: 1 to 16 replace_headers required, got %d", len(in))
	}
	seen := map[string]bool{}
	var out []Header
	for _, h := range in {
		name := h.GetHeader()
		if !headerRE.MatchString(name) {
			return nil, fmt.Errorf("header %q is not a header name", name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, fmt.Errorf("header %q repeated", name)
		}
		seen[key] = true
		if strings.ContainsAny(h.GetPrefix(), "\r\n\x00") {
			return nil, fmt.Errorf("header %q: prefix holds a control character", name)
		}
		c := Header{Name: canonical(name), Prefix: h.GetPrefix()}
		uri, jwt := h.GetCredentialUri(), h.GetActorJwt()
		switch {
		case uri != "" && jwt != nil, uri == "" && jwt == nil:
			return nil, fmt.Errorf("header %q: exactly one of credential_uri and actor_jwt", name)
		case uri != "":
			if !strings.HasPrefix(uri, "ate-secret://") {
				return nil, fmt.Errorf("header %q: credential_uri must be ate-secret://<class>/<name>/<tail>", name)
			}
			c.CredentialURI = uri
		default:
			if n := len(jwt.GetAudiences()); n == 0 || n > 16 {
				return nil, fmt.Errorf("header %q: actor_jwt needs 1 to 16 audiences", name)
			}
			if s := jwt.GetExpirationSeconds(); s < 300 || s > 3600 {
				return nil, fmt.Errorf("header %q: actor_jwt expiration %d outside 300..3600", name, s)
			}
			c.JWT = &JWTSource{Audiences: append([]string(nil), jwt.GetAudiences()...), ExpirationSeconds: jwt.GetExpirationSeconds()}
		}
		out = append(out, c)
	}
	return out, nil
}

func canonical(h string) string { return textproto.CanonicalMIMEHeaderKey(h) }

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidPattern reports whether h is a hostname pattern in Substrate's syntax: a lower-case RFC 1123 DNS name without
// a trailing dot, optionally with "*" as its whole leftmost label, or "*" alone. IP addresses are not names.
func ValidPattern(h string) error {
	if h == "*" {
		return nil
	}
	name := strings.TrimPrefix(h, "*.")
	if len(name) == 0 || len(name) > 253 {
		return fmt.Errorf("hostname %q: length", h)
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return fmt.Errorf("hostname %q: an IP address is not a hostname", h)
	}
	for _, l := range strings.Split(name, ".") {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("hostname %q: label %q is not a lower-case DNS label", h, l)
		}
	}
	return nil
}

// NormalizeName lower-cases a destination name and drops one trailing dot. It answers false for anything that is not
// a DNS name (an IP literal, a name with a port, an empty name); such a destination matches no rule.
func NormalizeName(name string) (string, bool) {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	if n == "" || n == "*" || strings.HasPrefix(n, "*.") || ValidPattern(n) != nil {
		return "", false
	}
	return n, true
}

// specificity of a pattern matching name: 2 exact, 1 wildcard, 0 no match.
func match(pattern, name string) int {
	if pattern == name {
		return 2
	}
	if pattern == "*" {
		return 1
	}
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		head, ok := strings.CutSuffix(name, suffix)
		if ok && head != "" && !strings.Contains(head, ".") {
			return 1
		}
	}
	return 0
}

func (r *Rule) portRank(port int) int {
	if r.Ports == nil {
		return 1
	}
	for _, p := range r.Ports {
		if p == port {
			return 2
		}
	}
	return 0
}

// Decide picks the rule that decides input for the destination name and port, or refuses. name is matched only
// after NormalizeName; an IP literal matches nothing.
func (p *Policy) Decide(in Input, name string, port int) Decision {
	n, ok := NormalizeName(name)
	if !ok {
		return Decision{Reason: fmt.Sprintf("destination %q is not a DNS name", name)}
	}
	var best *Rule
	bestName, bestPort := 0, 0
	for _, r := range p.Rules() {
		if !in.decidedBy(r.Protocol) {
			continue
		}
		pr := r.portRank(port)
		if pr == 0 {
			continue
		}
		nr := 0
		for _, pat := range r.Patterns {
			nr = max(nr, match(pat, n))
		}
		if nr == 0 {
			continue
		}
		if nr > bestName || (nr == bestName && pr > bestPort) {
			best, bestName, bestPort = r, nr, pr
		}
	}
	if best == nil {
		return Decision{Reason: fmt.Sprintf("no rule allows %s to %s:%d", in, n, port)}
	}
	return Decision{Allowed: true, Rule: best}
}

func (in Input) String() string {
	switch in {
	case InputHTTPRequest:
		return "http request"
	case InputTLSHello:
		return "tls"
	case InputInterceptedRequest:
		return "intercepted https request"
	}
	return "unknown input"
}

// checkTies refuses two rules deciding on the same input with the same pattern on the same port, or both on all
// ports.
func (p *Policy) checkTies() error {
	type key struct {
		group   int
		pattern string
		port    int
	}
	group := func(pr Protocol) int {
		if pr == ProtocolHTTP {
			return 1
		}
		return 2
	}
	seen := map[key]int{}
	for i, r := range p.rules {
		ports := r.Ports
		if ports == nil {
			ports = []int{0}
		}
		for _, pat := range r.Patterns {
			for _, port := range ports {
				k := key{group(r.Protocol), pat, port}
				if j, ok := seen[k]; ok && j != i {
					return fmt.Errorf("%w: rules[%d] and rules[%d] tie on %q port %s", ErrInvalid, j, i, pat, portName(port))
				}
				seen[k] = i
			}
		}
	}
	return nil
}

func portName(p int) string {
	if p == 0 {
		return "all"
	}
	return fmt.Sprint(p)
}
