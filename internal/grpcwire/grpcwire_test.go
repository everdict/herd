package grpcwire_test

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/internal/grpcwire"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func serve(t *testing.T, srv *grpcwire.Server) (*grpcwire.Conn, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() { srv.Close(); <-done })
	c := grpcwire.NewConn(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
	t.Cleanup(func() { c.Close() })
	return c, sock
}

func TestUnaryStatusAndDeadline(t *testing.T) {
	srv := grpcwire.NewServer()
	srv.Handle("/t.S/Echo", grpcwire.Unary(func(ctx context.Context, d *durationpb.Duration) (*durationpb.Duration, error) {
		if d.AsDuration() < 0 {
			return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "negative: 100%% sure\nreally")
		}
		if d.AsDuration() > time.Hour {
			<-ctx.Done() // the client's grpc-timeout ends this
			return nil, ctx.Err()
		}
		return d, nil
	}))
	c, _ := serve(t, srv)
	ctx := context.Background()
	out := new(durationpb.Duration)
	if err := c.Invoke(ctx, "/t.S/Echo", durationpb.New(3*time.Second), out, nil); err != nil || out.AsDuration() != 3*time.Second {
		t.Fatalf("Echo = %v, %v", out, err)
	}
	err := c.Invoke(ctx, "/t.S/Echo", durationpb.New(-1), out, nil)
	if grpcwire.CodeOf(err) != grpcwire.InvalidArgument || !strings.Contains(err.Error(), "100% sure\nreally") {
		t.Errorf("error = %v, want InvalidArgument with the message intact", err)
	}
	err = c.Invoke(ctx, "/t.S/Nope", durationpb.New(0), out, nil)
	if grpcwire.CodeOf(err) != grpcwire.Unimplemented {
		t.Errorf("unknown method = %v, want Unimplemented", err)
	}
	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = c.Invoke(dctx, "/t.S/Echo", durationpb.New(2*time.Hour), out, nil)
	if c := grpcwire.CodeOf(err); c != grpcwire.DeadlineExceeded || time.Since(start) > 5*time.Second {
		t.Errorf("slow call = %v after %v, want DeadlineExceeded soon", err, time.Since(start))
	}
}

func TestServerStreamAndNonGRPCRequests(t *testing.T) {
	srv := grpcwire.NewServer()
	srv.Handle("/t.S/Count", func(s *grpcwire.ServerStream) error {
		req := new(durationpb.Duration)
		if err := s.Recv(req); err != nil {
			return err
		}
		for i := int64(0); i < req.GetSeconds(); i++ {
			if err := s.Send(&timestamppb.Timestamp{Seconds: i}); err != nil {
				return err
			}
		}
		return grpcwire.Errorf(grpcwire.Aborted, "stopped after %d", req.GetSeconds())
	})
	c, sock := serve(t, srv)
	s, err := c.NewStream(context.Background(), "/t.S/Count", map[string]string{"x-test": "1"})
	if err != nil {
		t.Fatal(err)
	}
	s.Send(&durationpb.Duration{Seconds: 3})
	s.CloseSend()
	var got []int64
	for {
		m := new(timestamppb.Timestamp)
		if err = s.Recv(m); err != nil {
			break
		}
		got = append(got, m.GetSeconds())
	}
	if len(got) != 3 || got[2] != 2 || grpcwire.CodeOf(err) != grpcwire.Aborted {
		t.Errorf("stream = %v then %v, want 0,1,2 then Aborted", got, err)
	}
	// Plain HTTP/1.1 is refused.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Post("http://x/t.S/Count", "application/json", strings.NewReader("{}"))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("non-gRPC request answered 200")
		}
	}
}
