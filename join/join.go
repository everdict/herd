// Package join is how a host becomes a herdlet with one token (spec R-D4, ADR 0001 A-20): herd-api mints a join
// token, and `herdlet join <herd-api> --token <t>` exchanges it, over one unary call on the same gRPC port, for a
// herdlet credential naming that worker, an egress credential for the host's herd-egress, and the herd-router
// tunnel address to dial (and, when herd-api keeps snapshots in S3, that store). The join token itself grants
// nothing else: it cannot open a link or a tunnel and reads nothing on the Control API.
package join

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/s3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The join call's gRPC names.
const (
	ServiceName = "herd.ext.v1.Join"
	Method      = "/" + ServiceName + "/Join"
)

// Request names the worker the host joins as.
type Request struct {
	Worker string `json:"worker"`
}

// Grant is what a valid join token is exchanged for.
type Grant struct {
	Worker string `json:"worker"`
	// Credential is the herdlet credential: it opens this worker's link to herd-api and its tunnel to herd-router.
	Credential string `json:"credential"`
	// EgressCredential lets the host's herd-egress read the actors' egress policies.
	EgressCredential string `json:"egressCredential"`
	// Tunnel is the herd-router tunnel address herdlets dial; empty when herd-api advertises none.
	Tunnel string `json:"tunnel,omitempty"`
	// Snapshots is herd-api's S3 snapshot store (endpoint, bucket, credentials) when it keeps snapshots in one: the
	// herdlet writes and reads snapshots there, so a joined host needs no shared filesystem. Nil when herd-api's
	// store is a directory.
	Snapshots *s3.Config `json:"snapshots,omitempty"`
	ExpiresAt int64      `json:"expiresAt"`
}

// Server answers joins. Its key is herd-api's token key.
type Server struct {
	Key   []byte
	Authn auth.Authenticator
	// Connected reports whether a worker of that name holds a link now; such a name cannot be joined again.
	Connected func(worker string) bool
	// Tunnel is advertised to every joining herdlet.
	Tunnel string
	// Snapshots, if set, is handed to every joining herdlet (Grant.Snapshots).
	Snapshots *s3.Config
	// TTL is the lifetime of the HS256 credentials a token join grants (default DefaultCredentialTTL), and NotAfter, if
	// set, their latest end (the break-glass allowance's): a break-glass credential is short-lived and bound to its
	// worker, whose registration it alone may make.
	TTL      time.Duration
	NotAfter time.Time
	// OnGrant, if set, sees every grant before it is answered.
	OnGrant func(Grant)
	Now     func() time.Time
}

var workerRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidWorker reports whether name can be a worker name (a DNS label).
func ValidWorker(name string) error {
	if !workerRE.MatchString(name) {
		return fmt.Errorf("join: worker name %q is not a DNS label", name)
	}
	return nil
}

// Join exchanges the caller's join token for a Grant.
func (s *Server) Join(ctx context.Context, req *Request) (*Grant, error) {
	var h string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			h = v[0]
		}
	}
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if (!ok || tok == "") && len(auth.PeerChain(ctx)) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "join: no node SVID and no join token")
	}
	// A node joins by its X.509-SVID (spiffe://<td>/herd/node/<worker>, ADR 0001 A-60), or, under the HS256
	// break-glass, by a join token.
	p, err := s.Authn.Authenticate(ctx, tok)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "join: %v", err)
	}
	switch {
	case p.Join:
	case p.Herdlet && p.SVID():
		if req.Worker != p.HerdletWorker() {
			return nil, status.Errorf(codes.PermissionDenied, "join: the node SVID %s is worker %q's, not %q's", p.Subject, p.HerdletWorker(), req.Worker)
		}
	default:
		return nil, status.Errorf(codes.PermissionDenied, "join: %q is neither a node SVID nor a join token", p.Subject)
	}
	if err := ValidWorker(req.Worker); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if s.Connected != nil && s.Connected(req.Worker) {
		return nil, status.Errorf(codes.AlreadyExists, "join: worker %q is connected; a join cannot take its name", req.Worker)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultCredentialTTL
	}
	end := now().Add(ttl)
	if !s.NotAfter.IsZero() && s.NotAfter.Before(end) {
		end = s.NotAfter
	}
	exp := end.Unix()
	g := &Grant{Worker: req.Worker, Tunnel: s.Tunnel, Snapshots: s.Snapshots, ExpiresAt: exp}
	if p.Join {
		// A join token is break-glass: its host links with the HS256 credentials it is granted. A node that joined by
		// its SVID dials with the SVID and is granted none.
		cred, err := auth.Sign(s.Key, auth.Claims{Subject: req.Worker, ExpiresAt: exp, Herdlet: true})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "join: %v", err)
		}
		egress, err := auth.Sign(s.Key, auth.Claims{Subject: "herd-egress@" + req.Worker, ExpiresAt: exp,
			Spaces: map[string]auth.Role{auth.AllSpaces: auth.RoleRead}})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "join: %v", err)
		}
		g.Credential, g.EgressCredential = cred, egress
	}
	if s.OnGrant != nil {
		s.OnGrant(*g)
	}
	return g, nil
}

// MintToken mints a join token valid for ttl. It is what `herd-api join-token` prints.
func MintToken(key []byte, ttl time.Duration) (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return auth.Sign(key, auth.Claims{Subject: "join-" + hex.EncodeToString(id[:]), ExpiresAt: time.Now().Add(ttl).Unix(), Join: true})
}

// DefaultCredentialTTL is how long the HS256 credentials of a token join live: a herdlet joins again with its join
// token once they end.
const DefaultCredentialTTL = 24 * time.Hour

// Register serves the join call on g.
func (s *Server) Register(g *grpc.Server) {
	g.RegisterService(&grpc.ServiceDesc{
		ServiceName: ServiceName,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Join", Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var req Request
			if err := dec(&req); err != nil {
				return nil, err
			}
			return srv.(*Server).Join(ctx, &req)
		}}},
	}, s)
}

// Call exchanges token for a Grant at the herd-api cc reaches.
func Call(ctx context.Context, cc grpc.ClientConnInterface, token, worker string) (Grant, error) {
	opts := []grpc.CallOption{grpc.CallContentSubtype(link.CodecName)}
	if token != "" {
		opts = append(opts, grpc.PerRPCCredentials(auth.Bearer(token)))
	}
	var g Grant
	if err := cc.Invoke(ctx, Method, &Request{Worker: worker}, &g, opts...); err != nil {
		return Grant{}, err
	}
	if g.Worker != worker || (token != "" && g.Credential == "") {
		return Grant{}, fmt.Errorf("join: herd-api granted %q, not a grant for %q", g.Worker, worker)
	}
	return g, nil
}
