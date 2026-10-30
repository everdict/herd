//go:build !linux

package transport

import (
	"fmt"
	"net"
	"runtime"
)

// ListenVsock is available on linux only, where herd-guest runs.
func ListenVsock(port uint32) (net.Listener, error) {
	return nil, fmt.Errorf("transport: vsock is not available on %s", runtime.GOOS)
}
