package egress_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
)

// TestServeTransparent: the Ubuntu listener maps a flow's source address to its actor and decides on the original
// destination's port; a source bound to no actor is refused.
func TestServeTransparent(t *testing.T) {
	w := newWorld(t)
	w.resolve("api.listed.example", []string{"198.51.100.70"})
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, httpRule([]int32{8080}, "api.listed.example")))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var src egress.Sources
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = g.ServeTransparent(ctx, ln, &src, func(net.Conn) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("198.51.100.70:8080"), nil
		})
	}()
	do := func() (int, error) {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: api.listed.example\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufioReader(c), nil)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if code, err := do(); err == nil && code == 200 {
		t.Errorf("unbound source reached the upstream")
	}
	src.Set(netip.MustParseAddr("127.0.0.1"), actor1)
	if code, err := do(); err != nil || code != 200 {
		t.Errorf("bound source: %d %v, want 200", code, err)
	}
	src.Remove(netip.MustParseAddr("127.0.0.1"))
	if code, err := do(); err == nil && code == 200 {
		t.Errorf("removed source reached the upstream")
	}
}

func TestSockOriginalDstNotRedirected(t *testing.T) {
	a, _ := tcpPair(t)
	_, err := egress.SockOriginalDst(a)
	if runtime.GOOS == "linux" && err == nil {
		// Without conntrack NAT the kernel has no original destination for this socket.
		t.Log("SO_ORIGINAL_DST answered on a loopback connection (conntrack loaded)")
	}
	if _, err := egress.SockOriginalDst(nil); err == nil {
		t.Errorf("SockOriginalDst(nil) answered nil error")
	}
}
