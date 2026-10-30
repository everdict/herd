package machine

import (
	"context"
	"net"
	"time"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/guestclient"
)

// Flusher asks a VM's guest to write what it buffered back to its disks and its share (PLT-195), so a snapshot taken
// right after holds it.
type Flusher interface {
	Flush(ctx context.Context, vmID string) error
}

// DefaultFlushTimeout bounds a flush when Config.FlushTimeout is zero.
const DefaultFlushTimeout = 20 * time.Second

// GuestFlush is the default Flusher: herd-guest's Sync, over the guest dialer.
type GuestFlush struct {
	Guests GuestDialer
	// Port is herd-guest's API port (8080).
	Port uint32
}

func (f GuestFlush) Flush(ctx context.Context, vmID string) error {
	c, err := guestclient.New(func(ctx context.Context) (net.Conn, error) { return f.Guests.DialGuest(ctx, vmID, f.Port) })
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Invoke(ctx, agent.MethodSync, &herdext.SyncRequest{}, &herdext.SyncResponse{}, nil)
}
