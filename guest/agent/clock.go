package agent

import (
	"context"
	"log"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetClock steps the guest's wall clock to the host's (DEFAUL-1595): a guest restored from a MEMORY snapshot keeps the
// clock its memory held, so herdlet calls this before the VM serves anything. Only CLOCK_REALTIME moves, forwards or
// backwards (a host whose clock was stepped back is followed): CLOCK_MONOTONIC, which Go's timers and durations read,
// does not, so the process timeouts this guest arms keep their deadlines. The step is logged.
func (g *Guest) SetClock(_ context.Context, req *herdext.SetClockRequest) (*herdext.SetClockResponse, error) {
	if req.GetNow() == nil {
		return nil, status.Error(codes.InvalidArgument, "set clock: no time")
	}
	if err := req.GetNow().CheckValid(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set clock: %v", err)
	}
	now := req.GetNow().AsTime()
	set := g.SetTime
	if set == nil {
		set = setTime
	}
	before, err := set(now)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set clock to %s: %v", now.Format(time.RFC3339Nano), err)
	}
	log.Printf("herd-guest: wall clock set to the host's %s, stepped %v", now.Format(time.RFC3339Nano), now.Sub(before))
	return &herdext.SetClockResponse{Before: timestamppb.New(before)}, nil
}
