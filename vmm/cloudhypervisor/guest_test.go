//go:build linux

package cloudhypervisor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
)

// TestDialGuestSpeaksHybridVsock: DialGuest connects to the VM's vsock socket, asks for the port with CONNECT and
// hands back the stream after the OK line, the way Cloud Hypervisor's hybrid vsock answers.
func TestDialGuestSpeaksHybridVsock(t *testing.T) {
	state, err := os.MkdirTemp("", "chg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	v := New(Config{StateDir: state})
	dir := v.vmDir("vm-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "vsock.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	asked := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		asked <- line
		_, _ = io.WriteString(c, "OK 1073741824\nguest bytes")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := v.DialGuest(ctx, "vm-1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := <-asked; got != "CONNECT 8080\n" {
		t.Errorf("handshake %q", got)
	}
	b, _ := io.ReadAll(c)
	if string(b) != "guest bytes" {
		t.Errorf("stream %q", b)
	}
	if _, err := v.DialGuest(ctx, "../x", 8080); !errors.Is(err, vmm.ErrInvalidSpec) {
		t.Errorf("DialGuest with a path in the id: %v", err)
	}
}
