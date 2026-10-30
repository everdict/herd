// Package transport is herd-guest's connection boundary. The guest serves its API on a net.Listener: AF_VSOCK in a
// microVM (the guest side looks the same under Cloud Hypervisor's hybrid vsock and Virtualization.framework's native
// vsock; the difference is only on the host), and a unix socket in tests and when herd-guest runs outside a VM. On
// the host, Cloud Hypervisor's hybrid vsock is a unix socket plus a CONNECT handshake (DialHybridVsock).
package transport

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Listen opens a listener from an address: "vsock://:<port>" (any CID) or "unix://<path>".
func Listen(addr string) (net.Listener, error) {
	switch {
	case strings.HasPrefix(addr, "vsock://"):
		rest := strings.TrimPrefix(addr, "vsock://")
		i := strings.LastIndexByte(rest, ':')
		if i < 0 {
			return nil, fmt.Errorf("transport: %q: want vsock://:<port>", addr)
		}
		port, err := strconv.ParseUint(rest[i+1:], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("transport: %q: bad port: %v", addr, err)
		}
		return ListenVsock(uint32(port))
	case strings.HasPrefix(addr, "unix://"):
		return ListenUnix(strings.TrimPrefix(addr, "unix://"))
	}
	return nil, fmt.Errorf("transport: %q: want vsock://:<port> or unix://<path>", addr)
}

// ListenUnix listens on a unix socket, replacing a stale socket file left by an earlier guest.
func ListenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("transport: removing stale socket %s: %w", path, err)
		}
	}
	return net.Listen("unix", path)
}

// DialUnix returns a dialer for a unix socket.
func DialUnix(path string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

// DialHybridVsock returns a host-side dialer for a guest port behind Cloud Hypervisor's hybrid vsock: connect to
// the VM's vsock unix socket, send "CONNECT <port>\n", and expect "OK <host port>\n" before the stream is the
// guest's.
func DialHybridVsock(socketPath string, port uint32) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", socketPath)
		if err != nil {
			return nil, err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(dl)
		}
		if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
			c.Close()
			return nil, fmt.Errorf("transport: hybrid vsock CONNECT: %w", err)
		}
		// Read the answer byte by byte so no guest bytes are consumed past the newline.
		var line []byte
		one := make([]byte, 1)
		for len(line) < 64 {
			if _, err := c.Read(one); err != nil {
				c.Close()
				return nil, fmt.Errorf("transport: hybrid vsock answer: %w", err)
			}
			if one[0] == '\n' {
				break
			}
			line = append(line, one[0])
		}
		if !strings.HasPrefix(string(line), "OK ") {
			c.Close()
			return nil, fmt.Errorf("transport: hybrid vsock refused port %d: %q", port, line)
		}
		_ = c.SetDeadline(time.Time{})
		return c, nil
	}
}
