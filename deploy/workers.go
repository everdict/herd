package deploy

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"google.golang.org/grpc"
)

// Workers is `herd-api workers`: it lists the workers herd-api has registrations of and what each declared.
type Workers struct {
	API       string
	Token     string
	TokenFile string
	// Identity is the caller's SPIFFE identity (an operator's, the Workload API's by default): the call is its
	// X.509-SVID over TLS, or the bearer given on top of it.
	Identity Identity
}

// Flags registers the workers command's flags.
func (w *Workers) Flags(fs *flag.FlagSet) {
	fs.StringVar(&w.API, "api", "127.0.0.1:7443", "herd-api address")
	fs.StringVar(&w.Token, "token", "", "bearer (a JWT-SVID for herd-api, or a break-glass HS256 token); without it the call is this process's X.509-SVID")
	fs.StringVar(&w.TokenFile, "token-file", "", "file holding the bearer")
	w.Identity.Flags(fs)
}

// List returns one line per worker.
func (w *Workers) List(ctx context.Context) ([]string, error) {
	tok := w.Token
	if tok == "" && w.TokenFile != "" {
		b, err := os.ReadFile(w.TokenFile)
		if err != nil {
			return nil, err
		}
		tok = strings.TrimSpace(string(b))
	}
	src, closeID, err := w.Identity.Open(ctx, "herd-api workers")
	if err != nil {
		return nil, err
	}
	defer closeID()
	self, err := auth.Self(src)
	if err != nil {
		return nil, err
	}
	var opts []grpc.DialOption
	if tok != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(auth.Bearer(tok)))
	}
	cc, err := dial(w.API, src, auth.HerdID(self.TrustDomain(), "api"), opts...)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	var resp herdext.ListWorkerCapabilitiesResponse
	if err := cc.Invoke(ctx, "/herd.ext.v1.Capabilities/ListWorkerCapabilities", &herdext.ListWorkerCapabilitiesRequest{}, &resp); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range resp.GetWorkers() {
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", c.GetWorker(), c.GetPlatform(), c.GetArch(), c.GetSuspendMaxFidelity(), c.GetRestoreFamily())
		if p := c.GetEgressAllowPrefixes(); len(p) > 0 {
			line += "\tLAB egress-allow-prefix=" + strings.Join(p, ",")
		}
		out = append(out, line)
	}
	return out, nil
}
