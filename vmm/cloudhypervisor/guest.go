//go:build linux

package cloudhypervisor

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"github.com/everdict/herd/guest/transport"
	"github.com/everdict/herd/vmm"
)

// DialGuest connects to port in VM id's guest through Cloud Hypervisor's hybrid vsock: the VM's vsock socket and a
// CONNECT handshake. It makes the backend the herdlet's guest dialer for herd-router's tunnel.
func (v *VMM) DialGuest(ctx context.Context, id string, port uint32) (net.Conn, error) {
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("%w: id %q", vmm.ErrInvalidSpec, id)
	}
	return transport.DialHybridVsock(filepath.Join(v.vmDir(id), "vsock.sock"), port)(ctx)
}
