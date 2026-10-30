package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/transport"
	"github.com/everdict/herd/internal/grpcwire"
)

// The test binary is herd-guest when HERD_GUEST_MAIN is set (the shims it starts inherit it).
func TestMain(m *testing.M) {
	if os.Getenv("HERD_GUEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func herdGuest(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "HERD_GUEST_MAIN=1")
	return cmd
}

func startServe(t *testing.T, root, sock string) *exec.Cmd {
	t.Helper()
	cmd := herdGuest("serve", "-local", root, "-listen", "unix://"+sock)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 500; i++ {
		if c, err := transport.DialUnix(sock)(context.Background()); err == nil {
			c.Close()
			return cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("herd-guest did not start listening")
	return nil
}

// The herd-guest process is killed outright mid-exec; a new one re-follows the exec from the client's offsets and
// reports its exit.
func TestKilledGuestProcessIsRefollowedByTheNextOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	sock := filepath.Join(t.TempDir(), "g.sock")
	first := startServe(t, root, sock)
	conn := grpcwire.NewConn(transport.DialUnix(sock))
	p := new(ateenv.Process)
	err := conn.Invoke(ctx, agent.MethodStartProcess, &ateenv.StartProcessRequest{
		Command: []string{"sh", "-c", `i=0; while [ $i -lt 30 ]; do i=$((i+1)); echo "n$i"; sleep 0.02; done; exit 4`},
	}, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	read := func(c *grpcwire.Conn, stopAt int) *ateenv.Process {
		s, err := c.NewStream(ctx, agent.MethodStreamProcessOutput, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.Send(&ateenv.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), StdoutOffset: int64(got.Len()), Follow: true})
		s.CloseSend()
		for {
			m := new(ateenv.ProcessOutput)
			err := s.Recv(m)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				if stopAt > 0 {
					return nil // the guest died under the stream
				}
				t.Fatal(err)
			}
			if e := m.GetExit(); e != nil {
				return e
			}
			got.Write(m.GetStdout())
			if stopAt > 0 && got.Len() >= stopAt {
				return nil
			}
		}
	}
	read(conn, 12)
	first.Process.Kill()
	first.Wait()
	conn.Close()

	startServe(t, root, sock)
	conn = grpcwire.NewConn(transport.DialUnix(sock))
	defer conn.Close()
	exit := read(conn, 0)
	var want strings.Builder
	for i := 1; i <= 30; i++ {
		want.WriteString("n" + strconv.Itoa(i) + "\n")
	}
	if got.String() != want.String() {
		t.Errorf("stdout across a guest kill:\n got %q\nwant %q", got.String(), want.String())
	}
	if exit.GetExitCode() != 4 {
		t.Errorf("exit = %v, want 4", exit)
	}
}

func TestContainerInitExitsWithItsCommand(t *testing.T) {
	cmd := herdGuest("cinit", "--", "sh", "-c", "sleep 0.05 & exit 5")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 5 {
		t.Fatalf("cinit exit = %v, want 5", err)
	}
}

func TestSignalHelperRefusesInit(t *testing.T) {
	if err := herdGuest("signal", "1", "9").Run(); err == nil {
		t.Fatal("signal to process group 1 accepted")
	}
}
