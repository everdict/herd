package egress

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe"
)

const soOriginalDst = 80 // SO_ORIGINAL_DST, linux/netfilter_ipv4.h

// SockOriginalDst reads the pre-NAT destination of a connection nftables redirected (IPv4).
func SockOriginalDst(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("egress: SO_ORIGINAL_DST needs a TCP connection")
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var sa syscall.RawSockaddrInet4
	var serr error
	err = rc.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(sa))
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&l)), 0)
		if e != 0 {
			serr = e
		}
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	if serr != nil {
		return netip.AddrPort{}, serr
	}
	pb := (*[2]byte)(unsafe.Pointer(&sa.Port)) // network byte order whatever the host order
	port := uint16(pb[0])<<8 | uint16(pb[1])
	return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), port), nil
}
