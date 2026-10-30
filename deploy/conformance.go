package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/conformance/substrateapi"
	"google.golang.org/grpc/status"
)

// RunConformance is `herd conformance`: the suite against cfg's target, its report opened with the workers' lab
// egress allowances (-egress-allow-prefix), read from herd's capability extension, which the suite itself never
// reads; an egress case that passed under one reached a host-side address the operator opened.
func RunConformance(ctx context.Context, cfg substrateapi.Config) substrateapi.Report {
	header := egressHeader(ctx, cfg)
	rep := substrateapi.Run(ctx, cfg)
	rep.Header = append(header, rep.Header...)
	return rep
}

// egressHeader lists each worker's lab egress allowances; a target without herd's extension (Substrate) reports
// none.
func egressHeader(ctx context.Context, cfg substrateapi.Config) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := substrateapi.DialControl(cfg.Control, cfg.Token)
	if err != nil {
		return []string{fmt.Sprintf("egress allowances: not reported by the target (%v)", err)}
	}
	defer c.Close()
	var resp herdext.ListWorkerCapabilitiesResponse
	if err := c.Conn().Invoke(ctx, "/herd.ext.v1.Capabilities/ListWorkerCapabilities", &herdext.ListWorkerCapabilitiesRequest{}, &resp); err != nil {
		return []string{fmt.Sprintf("egress allowances: not reported by the target (%s)", status.Code(err))}
	}
	var out []string
	for _, w := range resp.GetWorkers() {
		if p := w.GetEgressAllowPrefixes(); len(p) > 0 {
			out = append(out, fmt.Sprintf("egress allowances: LAB -egress-allow-prefix %s on worker %s (host-side addresses herd-egress does not refuse)",
				strings.Join(p, ","), w.GetWorker()))
		}
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("egress allowances: none on %d workers", len(resp.GetWorkers())))
	}
	return out
}
