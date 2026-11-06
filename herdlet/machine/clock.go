package machine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/guestclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Clocker hands a VM's guest the host's wall clock (DEFAUL-1595). A guest restored from a MEMORY snapshot keeps the
// clock its memory held (Cloud Hypervisor also stops the guest's clock while it pauses a VM for a snapshot), so a
// golden taken days ago would start every session days behind.
type Clocker interface {
	// SetClock steps the guest's wall clock to the host's and answers how far it stepped (negative: backwards).
	SetClock(ctx context.Context, vmID string) (time.Duration, error)
}

// ErrClockUnsupported is answered for a guest whose herd-guest has no ClockService: one a MEMORY snapshot taken
// before it kept. Its clock is not set.
var ErrClockUnsupported = errors.New("the guest's herd-guest cannot set its clock (it predates herd.ext.v1.ClockService)")

// DefaultClockTimeout bounds a clock set when Config.ClockTimeout is zero.
const DefaultClockTimeout = 10 * time.Second

// GuestClock is the default Clocker: herd-guest's SetClock, over the guest dialer.
type GuestClock struct {
	Guests GuestDialer
	// Port is herd-guest's API port (8080).
	Port uint32
	// Now is the host's wall clock (time.Now if nil).
	Now func() time.Time
}

// SetClock dials the guest until it answers (as GuestFlush does) and only then reads the host's clock and calls
// SetClock, so the time the guest is handed is not stale by a dial's retries.
func (c GuestClock) SetClock(ctx context.Context, vmID string) (time.Duration, error) {
	conn, err := dialGuest(ctx, c.Guests, vmID, c.Port)
	if err != nil {
		return 0, err
	}
	var once sync.Once
	cl, err := guestclient.New(func(context.Context) (net.Conn, error) {
		var got net.Conn
		once.Do(func() { got = conn })
		if got == nil {
			return nil, fmt.Errorf("guest of %s: the connection was lost", vmID)
		}
		return got, nil
	})
	if err != nil {
		conn.Close()
		return 0, err
	}
	defer cl.Close()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	t := now()
	resp := &herdext.SetClockResponse{}
	err = cl.Invoke(ctx, agent.MethodSetClock, &herdext.SetClockRequest{Now: timestamppb.New(t)}, resp, nil)
	if status.Code(err) == codes.Unimplemented {
		return 0, fmt.Errorf("%w: %v", ErrClockUnsupported, err)
	}
	if err != nil {
		return 0, err
	}
	return t.Sub(resp.GetBefore().AsTime()), nil
}

// setClock hands the guest of vmID the host's clock, within the clock timeout. A guest that predates ClockService is
// logged as left on its snapshot's clock and not refused: refusing it would make every golden taken before
// DEFAUL-1595 unrestorable, while rebuilding the golden gives it the service.
func (m *Machines) setClock(ctx context.Context, vmID, what string) error {
	if m.cfg.Clock == nil {
		return nil
	}
	d := m.cfg.ClockTimeout
	if d <= 0 {
		d = DefaultClockTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	step, err := m.cfg.Clock.SetClock(ctx, vmID)
	if errors.Is(err, ErrClockUnsupported) {
		log.Printf("machine: %s: the guest's wall clock is NOT set and keeps its snapshot's: %v; rebuild the snapshot to fix it", what, err)
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("machine: %s: the guest's wall clock is the host's, stepped %v", what, step.Round(time.Millisecond))
	return nil
}
