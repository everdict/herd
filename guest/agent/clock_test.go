package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestSetClockStepsTheWallClock (DEFAUL-1595): SetClock steps the guest's wall clock to the host's time it is handed,
// backwards as well as forwards, answers the clock it replaced, and says so when the clock could not be set or no
// time came.
func TestSetClockStepsTheWallClock(t *testing.T) {
	g, err := agent.New(newTarget(t, "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := context.Background()
	guest := time.Date(2026, 11, 6, 7, 32, 18, 0, time.UTC)
	var set []time.Time
	g.SetTime = func(now time.Time) (time.Time, error) {
		before := guest
		guest = now
		set = append(set, now)
		return before, nil
	}
	host := time.Date(2026, 11, 6, 7, 44, 42, 123, time.UTC)
	resp, err := g.SetClock(ctx, &herdext.SetClockRequest{Now: timestamppb.New(host)})
	if err != nil {
		t.Fatal(err)
	}
	if !guest.Equal(host) || !resp.GetBefore().AsTime().Equal(time.Date(2026, 11, 6, 7, 32, 18, 0, time.UTC)) {
		t.Fatalf("guest clock %s (answered before %s), want the host's %s", guest, resp.GetBefore().AsTime(), host)
	}
	// A host whose clock was stepped back is followed.
	back := host.Add(-3 * time.Second)
	if _, err := g.SetClock(ctx, &herdext.SetClockRequest{Now: timestamppb.New(back)}); err != nil || !guest.Equal(back) {
		t.Fatalf("stepping back: guest clock %s, %v; want %s", guest, err, back)
	}
	if _, err := g.SetClock(ctx, &herdext.SetClockRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no time: %v, want InvalidArgument", err)
	}
	if len(set) != 2 {
		t.Fatalf("clock set %d times, want 2 (a call with no time sets nothing)", len(set))
	}
	g.SetTime = func(time.Time) (time.Time, error) { return guest, errors.New("operation not permitted") }
	if _, err := g.SetClock(ctx, &herdext.SetClockRequest{Now: timestamppb.New(host)}); status.Code(err) != codes.Internal {
		t.Fatalf("a refused set answered %v, want Internal", err)
	}
}
