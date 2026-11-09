package machine

import (
	"context"
	"fmt"
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

// A flush is bounded, when Config.FlushTimeout is zero, by FlushBase plus the VM's memory at FlushFloorRate
// (FlushTimeoutFor): the flush saves each container's writable layer, which lives in the guest's tmpfs, so the VM's
// memory bounds the work (PLT-378). 6 GiB: 116 s; 512 MiB: 28 s.
const (
	FlushBase      = 20 * time.Second
	FlushFloorRate = 64 << 20 // bytes per second
)

// FlushTimeoutFor is the bound of a flush of a VM with memoryMiB of memory when Config.FlushTimeout is zero.
func FlushTimeoutFor(memoryMiB int) time.Duration {
	return FlushBase + time.Duration(int64(memoryMiB)<<20*int64(time.Second)/FlushFloorRate)
}

// DialRetry is how long GuestFlush waits between dials of a guest that does not answer yet.
const DialRetry = 20 * time.Millisecond

// dialAttempt bounds one dial of the guest, so one hung handshake does not use up the whole flush.
const dialAttempt = 2 * time.Second

// GuestFlush is the default Flusher: herd-guest's Sync, over the guest dialer.
type GuestFlush struct {
	Guests GuestDialer
	// Port is herd-guest's API port (8080).
	Port uint32
}

// Flush dials the guest until it answers and then calls Sync; ctx bounds both. A hypervisor may answer a boot before
// herd-guest serves (Cloud Hypervisor does not wait for the guest), so a snapshot right after a boot can find a guest
// that is still starting: that guest is waited for, not taken for a failed flush (PLT-200).
func (f GuestFlush) Flush(ctx context.Context, vmID string) error {
	c, err := guestclient.New(func(ctx context.Context) (net.Conn, error) { return dialGuest(ctx, f.Guests, vmID, f.Port) })
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Invoke(ctx, agent.MethodSync, &herdext.SyncRequest{}, &herdext.SyncResponse{}, nil)
}

// dialGuest reaches port of the guest of vmID, trying again every DialRetry until ctx ends.
func dialGuest(ctx context.Context, guests GuestDialer, vmID string, port uint32) (net.Conn, error) {
	for {
		actx, cancel := context.WithTimeout(ctx, dialAttempt)
		conn, err := guests.DialGuest(actx, vmID, port)
		cancel()
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guest of %s did not answer on port %d: %w (last: %v)", vmID, port, ctx.Err(), err)
		case <-time.After(DialRetry):
		}
	}
}
