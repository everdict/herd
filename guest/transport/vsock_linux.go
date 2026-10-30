//go:build linux

package transport

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

const (
	afVsock      = 40         // AF_VSOCK
	vmaddrCIDAny = 0xffffffff // VMADDR_CID_ANY
)

// sockaddrVM is struct sockaddr_vm.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// VsockAddr is a vsock endpoint.
type VsockAddr struct{ CID, Port uint32 }

func (a VsockAddr) Network() string { return "vsock" }
func (a VsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.CID, a.Port) }

// ListenVsock listens on an AF_VSOCK stream port for any CID. The standard library has no vsock sockaddr, so the
// socket is set up with raw system calls and driven through the runtime poller as an *os.File.
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("transport: vsock socket: %w", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: vmaddrCIDAny}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("transport: vsock bind port %d: %w", port, e)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("transport: vsock listen: %w", err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock-listener"), addr: VsockAddr{CID: vmaddrCIDAny, Port: port}}, nil
}

type vsockListener struct {
	f    *os.File
	addr VsockAddr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var aerr error
	var peer sockaddrVM
	err = rc.Read(func(fd uintptr) bool {
		n := uint32(unsafe.Sizeof(peer))
		r, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, fd, uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&n)), syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
		if e == syscall.EAGAIN {
			return false
		}
		nfd, aerr = int(r), nil
		if e != 0 {
			aerr = e
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, fmt.Errorf("transport: vsock accept: %w", aerr)
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), local: l.addr, remote: VsockAddr{CID: peer.cid, Port: peer.port}}, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn is an accepted vsock stream; *os.File gives it Read, Write, Close and deadlines via the poller.
type vsockConn struct {
	*os.File
	local, remote VsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }
