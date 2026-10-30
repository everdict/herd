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
	"google.golang.org/grpc/credentials/insecure"
)

// Workers is `herd-api workers`: it lists the workers herd-api has registrations of and what each declared.
type Workers struct {
	API       string
	Token     string
	TokenFile string
}

// Flags registers the workers command's flags.
func (w *Workers) Flags(fs *flag.FlagSet) {
	fs.StringVar(&w.API, "api", "127.0.0.1:7443", "herd-api address")
	fs.StringVar(&w.Token, "token", "", "bearer token (any valid client token)")
	fs.StringVar(&w.TokenFile, "token-file", "", "file holding the bearer token")
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
	cc, err := grpc.NewClient("passthrough:///"+w.API, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(tok)))
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
		out = append(out, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", c.GetWorker(), c.GetPlatform(), c.GetArch(), c.GetSuspendMaxFidelity(), c.GetRestoreFamily()))
	}
	return out, nil
}
