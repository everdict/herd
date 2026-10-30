package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
)

// Flow is one TCP connection a VM opened, as a userspace stack terminates it: the stack's end of the connection and
// the address the VM connected to.
type Flow struct {
	Conn net.Conn
	Dst  netip.AddrPort
}

// Stack is a userspace network stack serving one VM's network device. On macOS that device is a
// VZFileHandleNetworkDeviceAttachment whose other end (a datagram socket carrying Ethernet frames) the stack reads;
// the stack is gvisor-tap-vsock's, wired so that every TCP flow comes out of Accept and nothing else leaves:
//   - TCP to any address and port is terminated in the stack and handed to herd-egress as a Flow;
//   - UDP to port 53 at the gateway address is answered by the StackConfig's DNS function; all other UDP, ICMP and
//     IPv6 is dropped and counted.
//
// The VZ half (gvisor-tap-vsock and the attachment) lands with R7 (PLT-118); vmm/vz attaches VMFile. egress/stacktest
// holds a fake with the same contract.
type Stack interface {
	// Accept returns the VM's next TCP flow; it fails once the stack is closed.
	Accept(ctx context.Context) (Flow, error)
	// Dropped counts packets the stack dropped because they were not TCP or DNS.
	Dropped() int64
	Close() error
}

// StackConfig is what a Stack needs from herd-egress.
type StackConfig struct {
	// Gateway is the address the VM's default route and DNS server point at.
	Gateway netip.Addr
	// VM is the address the stack hands the VM (DHCP or static).
	VM netip.Addr
	// DNS answers one DNS query message.
	DNS func(query []byte) ([]byte, error)
}

// StackFactory makes a Stack for one VM and returns the file the hypervisor attaches as the VM's network device.
type StackFactory interface {
	NewStack(actor Actor, cfg StackConfig) (Stack, *os.File, error)
}

// ServeStack serves every flow of actor's stack through the gateway until the stack closes or ctx ends.
func (g *Gateway) ServeStack(ctx context.Context, actor Actor, s Stack) error {
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	for {
		f, err := s.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go g.ServeConn(ctx, actor, f.Conn, f.Dst)
	}
}
