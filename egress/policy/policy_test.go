package policy_test

import (
	"errors"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/egress/policy"
)

func h(ports []int32, hosts ...string) *ateapi.EgressRule {
	r := &ateapi.HttpRule{Hostnames: hosts}
	if ports != nil {
		r.Ports = &ateapi.Ports{Numbers: ports}
	}
	return &ateapi.EgressRule{Http: r}
}

func hs(ports []int32, hosts ...string) *ateapi.EgressRule {
	r := &ateapi.HttpsRule{Hostnames: hosts}
	if ports != nil {
		r.Ports = &ateapi.Ports{Numbers: ports}
	}
	return &ateapi.EgressRule{Https: r}
}

func pt(ports []int32, hosts ...string) *ateapi.EgressRule {
	p := &ateapi.Ports{Numbers: ports}
	if ports == nil {
		p = &ateapi.Ports{All: &ateapi.AllPorts{}}
	}
	return &ateapi.EgressRule{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: hosts, Ports: p}}
}

func mustCompile(t *testing.T, rules ...*ateapi.EgressRule) *policy.Policy {
	t.Helper()
	p, err := policy.CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultDeny(t *testing.T) {
	for _, p := range []*policy.Policy{nil, policy.Deny, mustCompile(t)} {
		if d := p.Decide(policy.InputHTTPRequest, "example.com", 80); d.Allowed {
			t.Errorf("empty policy allowed: %+v", d)
		}
	}
	p, err := policy.Compile(nil)
	if err != nil || p != policy.Deny {
		t.Errorf("Compile(nil) = %v, %v", p, err)
	}
}

func TestMatching(t *testing.T) {
	p := mustCompile(t,
		h(nil, "api.example.com"),
		h([]int32{8080}, "*.example.com"),
		hs(nil, "*.example.com"),
		pt([]int32{8443}, "db.example.com"),
		pt(nil, "*.passthrough.example"),
	)
	cases := []struct {
		in    policy.Input
		name  string
		port  int
		allow bool
		proto policy.Protocol
	}{
		{policy.InputHTTPRequest, "api.example.com", 80, true, policy.ProtocolHTTP},
		{policy.InputHTTPRequest, "API.Example.COM.", 80, true, policy.ProtocolHTTP},
		{policy.InputHTTPRequest, "api.example.com", 8081, false, 0},
		{policy.InputHTTPRequest, "x.example.com", 8080, true, policy.ProtocolHTTP},
		{policy.InputHTTPRequest, "x.example.com", 80, false, 0}, // wildcard rule is on 8080 only
		{policy.InputHTTPRequest, "a.b.example.com", 8080, false, 0},
		{policy.InputHTTPRequest, "example.com", 8080, false, 0}, // "*." needs one label
		{policy.InputTLSHello, "api.example.com", 443, true, policy.ProtocolHTTPS},
		{policy.InputTLSHello, "api.example.com", 80, false, 0}, // http rule does not allow TLS
		{policy.InputHTTPRequest, "api.example.com", 443, false, 0},
		{policy.InputTLSHello, "db.example.com", 8443, true, policy.ProtocolTLSPassthrough},
		{policy.InputTLSHello, "db.example.com", 443, true, policy.ProtocolHTTPS}, // wildcard https on 443
		{policy.InputInterceptedRequest, "db.example.com", 8443, false, 0},        // passthrough does not decide requests
		{policy.InputInterceptedRequest, "x.example.com", 443, true, policy.ProtocolHTTPS},
		{policy.InputTLSHello, "q.passthrough.example", 1, true, policy.ProtocolTLSPassthrough},
		{policy.InputTLSHello, "q.passthrough.example", 65535, true, policy.ProtocolTLSPassthrough},
		{policy.InputHTTPRequest, "1.2.3.4", 80, false, 0},
		{policy.InputHTTPRequest, "", 80, false, 0},
		{policy.InputHTTPRequest, "*.example.com", 8080, false, 0},
	}
	for _, c := range cases {
		d := p.Decide(c.in, c.name, c.port)
		if d.Allowed != c.allow || (d.Allowed && d.Rule.Protocol != c.proto) {
			proto := policy.ProtocolNone
			if d.Rule != nil {
				proto = d.Rule.Protocol
			}
			t.Errorf("Decide(%v, %q, %d) = %v/%v (%s), want %v/%v", c.in, c.name, c.port, d.Allowed, proto, d.Reason, c.allow, c.proto)
		}
	}
}

func TestPrecedence(t *testing.T) {
	// An exact name beats a wildcard; then a named port beats all ports. Across https and tls_passthrough too.
	p := mustCompile(t,
		pt(nil, "*.example.com"),
		hs(nil, "api.example.com"),
		pt(nil, "db.example.com"),
		pt([]int32{443}, "*.wild.example"),
		&ateapi.EgressRule{Https: &ateapi.HttpsRule{Hostnames: []string{"*.wild.example"}, Ports: &ateapi.Ports{All: &ateapi.AllPorts{}}}},
	)
	for _, c := range []struct {
		name  string
		port  int
		proto policy.Protocol
	}{
		{"api.example.com", 443, policy.ProtocolHTTPS},
		{"other.example.com", 443, policy.ProtocolTLSPassthrough},
		{"db.example.com", 443, policy.ProtocolTLSPassthrough},
		{"x.wild.example", 443, policy.ProtocolTLSPassthrough},
		{"x.wild.example", 444, policy.ProtocolHTTPS},
	} {
		d := p.Decide(policy.InputTLSHello, c.name, c.port)
		if !d.Allowed || d.Rule.Protocol != c.proto {
			t.Errorf("%s:%d decided by %+v, want %v", c.name, c.port, d.Rule, c.proto)
		}
	}
}

func TestInvalid(t *testing.T) {
	jwt := &ateapi.ActorJwtSource{Audiences: []string{"a"}, ExpirationSeconds: 600}
	bad := map[string][]*ateapi.EgressRule{
		"empty rule":              {{}},
		"two protocols":           {{Http: &ateapi.HttpRule{Hostnames: []string{"a.com"}}, Https: &ateapi.HttpsRule{Hostnames: []string{"a.com"}}}},
		"no hostnames":            {h(nil)},
		"ip hostname":             {h(nil, "169.254.169.254")},
		"ipv6 hostname":           {h(nil, "::1")},
		"upper case":              {h(nil, "API.example.com")},
		"trailing dot":            {h(nil, "api.example.com.")},
		"url":                     {h(nil, "https://api.example.com")},
		"name with port":          {h(nil, "api.example.com:443")},
		"double dot":              {h(nil, "foo..example.com")},
		"inner wildcard":          {h(nil, "api.*.example.com")},
		"partial wildcard":        {h(nil, "a*.example.com")},
		"passthrough no ports":    {{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: []string{"a.com"}}}},
		"port zero":               {h([]int32{0}, "a.com")},
		"port too big":            {h([]int32{65536}, "a.com")},
		"port repeated":           {h([]int32{80, 80}, "a.com")},
		"all and numbers":         {{Http: &ateapi.HttpRule{Hostnames: []string{"a.com"}, Ports: &ateapi.Ports{All: &ateapi.AllPorts{}, Numbers: []int32{1}}}}},
		"tie same port":           {h(nil, "a.com"), h([]int32{80}, "a.com")},
		"tie all ports":           {pt(nil, "a.com"), {Https: &ateapi.HttpsRule{Hostnames: []string{"a.com"}, Ports: &ateapi.Ports{All: &ateapi.AllPorts{}}}}},
		"tie https passthrough":   {hs(nil, "*.a.com"), pt([]int32{443}, "*.a.com")},
		"hostname repeated":       {h(nil, "a.com", "a.com")},
		"effects empty":           {{Http: &ateapi.HttpRule{Hostnames: []string{"a.com"}, Effects: &ateapi.HttpRuleEffects{}}}},
		"header both sources":     {effects(&ateapi.CredentialHeader{Header: "Authorization", CredentialUri: "ate-secret://x/y/z", ActorJwt: jwt})},
		"header no source":        {effects(&ateapi.CredentialHeader{Header: "Authorization"})},
		"header bad name":         {effects(&ateapi.CredentialHeader{Header: "Bad Header", CredentialUri: "ate-secret://x/y/z"})},
		"header dup case":         {effects(&ateapi.CredentialHeader{Header: "authorization", CredentialUri: "ate-secret://x/y/z"}, &ateapi.CredentialHeader{Header: "Authorization", CredentialUri: "ate-secret://x/y/z"})},
		"credential not ate":      {effects(&ateapi.CredentialHeader{Header: "Authorization", CredentialUri: "file:///etc/passwd"})},
		"jwt short":               {effects(&ateapi.CredentialHeader{Header: "X", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"a"}, ExpirationSeconds: 60}})},
		"jwt no audience":         {effects(&ateapi.CredentialHeader{Header: "X", ActorJwt: &ateapi.ActorJwtSource{ExpirationSeconds: 600}})},
		"prefix control char":     {effects(&ateapi.CredentialHeader{Header: "X", Prefix: "a\r\nInjected: 1", CredentialUri: "ate-secret://x/y/z"})},
		"label too long (64)":     {h(nil, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com")},
		"label starts with dash":  {h(nil, "-a.com")},
		"wildcard on empty":       {h(nil, "*.")},
		"unicode not punycode":    {h(nil, "bücher.example")},
		"underscore not RFC 1123": {h(nil, "a_b.example")},
	}
	for name, rules := range bad {
		if _, err := policy.CompileRules(rules); !errors.Is(err, policy.ErrInvalid) {
			t.Errorf("%s: CompileRules = %v, want ErrInvalid", name, err)
		}
	}
	ok := [][]*ateapi.EgressRule{
		{h(nil, "*")},
		{h(nil, "a.com"), hs(nil, "a.com")},            // different inputs
		{h(nil, "a.com"), h([]int32{8080}, "a.com")},   // different ports
		{h(nil, "a.com"), h(nil, "*.a.com")},           // different patterns
		{hs(nil, "a.com"), pt([]int32{8443}, "a.com")}, // different ports
		{h(nil, "xn--bcher-kva.example")},              // punycode
		{effects(&ateapi.CredentialHeader{Header: "X", ActorJwt: jwt})},
	}
	for i, rules := range ok {
		if _, err := policy.CompileRules(rules); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
}

func effects(hdrs ...*ateapi.CredentialHeader) *ateapi.EgressRule {
	return &ateapi.EgressRule{Https: &ateapi.HttpsRule{Hostnames: []string{"a.com"}, Effects: &ateapi.HttpRuleEffects{ReplaceHeaders: hdrs}}}
}

func TestHeadersCompiled(t *testing.T) {
	p := mustCompile(t, effects(
		&ateapi.CredentialHeader{Header: "authorization", Prefix: "Bearer ", CredentialUri: "ate-secret://vault/main/openai"},
		&ateapi.CredentialHeader{Header: "x-actor-token", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"aud"}, ExpirationSeconds: 900}},
	))
	hd := p.Rules()[0].Headers
	if len(hd) != 2 || hd[0].Name != "Authorization" || hd[0].Prefix != "Bearer " || hd[0].CredentialURI == "" ||
		hd[1].Name != "X-Actor-Token" || hd[1].JWT == nil || hd[1].JWT.ExpirationSeconds != 900 {
		t.Errorf("headers = %+v", hd)
	}
}
