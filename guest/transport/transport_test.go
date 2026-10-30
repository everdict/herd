package transport_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/guest/transport"
)

func echoOnce(t *testing.T, l net.Listener) {
	t.Helper()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
}

func TestUnixListenReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "g.sock")
	l1, err := transport.Listen("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	l1.(*net.UnixListener).SetUnlinkOnClose(false)
	l1.Close() // a crashed guest leaves its socket file behind
	l2, err := transport.Listen("unix://" + sock)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer l2.Close()
	echoOnce(t, l2)
	c, err := transport.DialUnix(sock)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ping\n"))
	if line, _ := bufio.NewReader(c).ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo = %q", line)
	}
}

// fakeHybridVsock plays Cloud Hypervisor's side of hybrid vsock: a unix socket that reads "CONNECT <port>\n" and
// answers "OK <host port>\n" for the open port, or closes for any other.
func fakeHybridVsock(t *testing.T, open uint32) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vsock.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				if line != "CONNECT "+itoa(open)+"\n" {
					return
				}
				// The guest's first bytes follow the OK line in the same write, as they may on a real host.
				c.Write([]byte("OK 1073741824\nguest-hello"))
				io.Copy(c, r)
			}()
		}
	}()
	return sock
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

func TestDialHybridVsock(t *testing.T) {
	sock := fakeHybridVsock(t, 8080)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := transport.DialHybridVsock(sock, 8080)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, len("guest-hello"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "guest-hello" {
		t.Fatalf("first guest bytes = %q, %v: the handshake must not eat them", buf, err)
	}
	if _, err := transport.DialHybridVsock(sock, 9)(ctx); err == nil {
		t.Fatal("dial of a closed port succeeded")
	}
}

func TestListenVsock(t *testing.T) {
	l, err := transport.Listen("vsock://:52011")
	if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPERM) || err != nil && strings.Contains(err.Error(), "not available") {
		t.Skipf("no AF_VSOCK on this kernel: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr().Network() != "vsock" {
		t.Errorf("network = %q", l.Addr().Network())
	}
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	l.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Accept after Close returned a connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
}

func TestListenRejectsUnknownAddress(t *testing.T) {
	for _, a := range []string{"tcp://:1", "vsock://8080", "vsock://:x"} {
		if _, err := transport.Listen(a); err == nil {
			t.Errorf("Listen(%q) succeeded", a)
		}
	}
}
