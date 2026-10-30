//go:build unix

package gvstack

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// socketPair returns a connected pair of datagram sockets: the stack's end as a net.Conn and the VM's end as a
// file. Virtualization.framework asks for large buffers on the VM's end (vfkit uses 1 MiB send, 4 MiB receive);
// a kernel that caps them is not an error.
func socketPair() (net.Conn, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	}
	hf := os.NewFile(uintptr(fds[0]), "herd-egress-stack")
	vm := os.NewFile(uintptr(fds[1]), "herd-vm-nic")
	host, err := net.FileConn(hf)
	_ = hf.Close()
	if err != nil {
		_ = vm.Close()
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	return host, vm, nil
}
