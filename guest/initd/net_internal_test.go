package initd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"syscall"
	"testing"
)

// The RTM_NEWROUTE message, byte by byte as struct nlmsghdr, struct rtmsg and two struct rtattr lay out on a
// little-endian host (amd64, arm64).
func TestRouteRequestBytes(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("expected bytes are little-endian")
	}
	got := routeRequest(7, netip.MustParseAddr("10.88.0.1"), 3)
	want := []byte{
		44, 0, 0, 0, // nlmsg_len
		24, 0, // nlmsg_type RTM_NEWROUTE
		0x05, 0x06, // nlmsg_flags NLM_F_REQUEST|NLM_F_ACK|NLM_F_EXCL|NLM_F_CREATE = 0x605
		7, 0, 0, 0, // nlmsg_seq
		0, 0, 0, 0, // nlmsg_pid
		2,          // rtm_family AF_INET
		0,          // rtm_dst_len: default route
		0,          // rtm_src_len
		0,          // rtm_tos
		254,        // rtm_table RT_TABLE_MAIN
		3,          // rtm_protocol RTPROT_BOOT
		0,          // rtm_scope RT_SCOPE_UNIVERSE
		1,          // rtm_type RTN_UNICAST
		0, 0, 0, 0, // rtm_flags
		8, 0, 5, 0, 10, 88, 0, 1, // RTA_GATEWAY 10.88.0.1
		8, 0, 4, 0, 3, 0, 0, 0, // RTA_OIF 3
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("routeRequest =\n% x\nwant\n% x", got, want)
	}
}

func TestParseAck(t *testing.T) {
	msg := func(typ uint16, seq uint32, errno int32) []byte {
		b := make([]byte, 36) // nlmsghdr, error, the request's header
		e := binary.NativeEndian
		e.PutUint32(b[0:], 36)
		e.PutUint16(b[4:], typ)
		e.PutUint32(b[8:], seq)
		e.PutUint32(b[16:], uint32(errno))
		return b
	}
	if err := parseAck(msg(nlmsgError, 1, 0), 1); err != nil {
		t.Errorf("ACK: %v", err)
	}
	if err := parseAck(msg(nlmsgError, 1, -int32(syscall.EEXIST)), 1); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("EEXIST: %v", err)
	}
	if err := parseAck(msg(nlmsgError, 1, -int32(syscall.ENETUNREACH)), 1); !errors.Is(err, syscall.ENETUNREACH) {
		t.Errorf("ENETUNREACH: %v", err)
	}
	if err := parseAck(msg(nlmsgError, 2, 0), 1); err == nil {
		t.Error("an answer to another request taken as the ACK")
	}
	if err := parseAck(msg(nlmsgError, 1, 0)[:10], 1); err == nil {
		t.Error("a truncated answer taken as the ACK")
	}
	if err := parseAck(append(msg(16, 1, 0), msg(nlmsgError, 1, -int32(syscall.EPERM))...), 1); !errors.Is(err, syscall.EPERM) {
		t.Errorf("error after another message: %v", err)
	}
}
