package grpcwire

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// Dialer opens one connection to the server (a unix socket, a vsock, a TCP address).
type Dialer func(ctx context.Context) (net.Conn, error)

// Conn is a client of one gRPC server over unencrypted HTTP/2. Calls share connections.
type Conn struct {
	hc *http.Client
	tr *http.Transport
}

// NewConn returns a client that reaches the server through dial.
func NewConn(dial Dialer) *Conn {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
	}
	return &Conn{hc: &http.Client{Transport: tr}, tr: tr}
}

// Close drops idle connections.
func (c *Conn) Close() error {
	c.tr.CloseIdleConnections()
	return nil
}

// ClientStream is the client side of one call.
type ClientStream struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
	resp   *http.Response
	err    error
	once   sync.Once
	ended  error
}

// NewStream starts a call. md is sent as request metadata.
func (c *Conn) NewStream(ctx context.Context, fullMethod string, md map[string]string) (*ClientStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://herd-guest"+fullMethod, pr)
	if err != nil {
		cancel()
		return nil, Errorf(Internal, "%v", err)
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("Te", "trailers")
	for k, v := range md {
		req.Header.Set(k, v)
	}
	if dl, ok := ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 && ms < 1e8 {
			req.Header.Set("Grpc-Timeout", strconv.FormatInt(ms, 10)+"m")
		}
	}
	s := &ClientStream{pw: pw, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		resp, err := c.hc.Do(req)
		if err != nil {
			s.err = err
			return
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			s.err = Errorf(Unknown, "HTTP status %d", resp.StatusCode)
			return
		}
		s.resp = resp
	}()
	return s, nil
}

// Send writes one request message.
func (s *ClientStream) Send(m proto.Message) error { return writeFrame(s.pw, m) }

// CloseSend tells the server no more request messages follow.
func (s *ClientStream) CloseSend() error { return s.pw.Close() }

// Recv reads the next response message. At the end of the stream it returns io.EOF if the call succeeded, or the
// call's *Status.
func (s *ClientStream) Recv(m proto.Message) error {
	<-s.done
	if s.err != nil {
		return s.transportErr(s.err)
	}
	if s.ended != nil {
		return s.ended
	}
	err := readFrame(s.resp.Body, m)
	if err == nil {
		return nil
	}
	if !errors.Is(err, io.EOF) {
		// A cut stream: the trailers never came.
		s.finish(err)
		return s.ended
	}
	// Trailers are readable once the body is drained; a Trailers-Only answer carries them in the headers.
	code, msg := s.resp.Trailer.Get("Grpc-Status"), s.resp.Trailer.Get("Grpc-Message")
	if code == "" {
		code, msg = s.resp.Header.Get("Grpc-Status"), s.resp.Header.Get("Grpc-Message")
	}
	st := parseStatus(code, msg)
	if st == nil {
		st = io.EOF
	}
	s.finish(st)
	return s.ended
}

func (s *ClientStream) finish(err error) {
	s.once.Do(func() {
		s.ended = err
		s.resp.Body.Close()
		s.cancel()
	})
}

func (s *ClientStream) transportErr(err error) error {
	var st *Status
	if errors.As(err, &st) {
		return st
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Status{Code: CodeOf(err), Message: err.Error()}
	}
	return Errorf(Unavailable, "%v", err)
}

// Close abandons the call.
func (s *ClientStream) Close() {
	s.cancel()
	s.pw.CloseWithError(context.Canceled)
}

// Invoke makes a unary call.
func (c *Conn) Invoke(ctx context.Context, fullMethod string, req, resp proto.Message, md map[string]string) error {
	s, err := c.NewStream(ctx, fullMethod, md)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Send(req); err != nil {
		return s.transportErr(err)
	}
	if err := s.CloseSend(); err != nil {
		return s.transportErr(err)
	}
	if err := s.Recv(resp); err != nil {
		if errors.Is(err, io.EOF) {
			return Errorf(Internal, "no response message")
		}
		return err
	}
	extra := resp.ProtoReflect().New().Interface()
	if err := s.Recv(extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Errorf(Internal, "more than one response message")
		}
		return err
	}
	return nil
}
