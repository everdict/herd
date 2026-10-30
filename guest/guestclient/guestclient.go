// Package guestclient is the host side of herd-guest's API: a grpc-go connection over a guest transport dialer
// (transport.DialUnix, transport.DialHybridVsock), with the per-call request metadata herd-guest reads
// (agent.ContainerHeader).
package guestclient

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Conn is a connection to one herd-guest.
type Conn struct{ cc *grpc.ClientConn }

// New returns a connection that reaches the guest through dial. No connection is made until the first call.
func New(dial func(ctx context.Context) (net.Conn, error)) (*Conn, error) {
	cc, err := grpc.NewClient("passthrough:///herd-guest",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dial(ctx) }))
	if err != nil {
		return nil, fmt.Errorf("guestclient: %w", err)
	}
	return &Conn{cc: cc}, nil
}

// ClientConn is the underlying grpc connection, for generated clients (e.g. env's ProcessServiceClient).
func (c *Conn) ClientConn() *grpc.ClientConn { return c.cc }

// Close closes the connection.
func (c *Conn) Close() error { return c.cc.Close() }

func withMD(ctx context.Context, md map[string]string) context.Context {
	if len(md) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, metadata.New(md))
}

// Invoke makes a unary call with request metadata md.
func (c *Conn) Invoke(ctx context.Context, fullMethod string, req, resp proto.Message, md map[string]string) error {
	return c.cc.Invoke(withMD(ctx, md), fullMethod, req, resp)
}

// Stream is one streaming call.
type Stream struct {
	cs     grpc.ClientStream
	cancel context.CancelFunc
}

// NewStream opens a streaming call (server-, client- or bidirectional-streaming) with request metadata md.
func (c *Conn) NewStream(ctx context.Context, fullMethod string, md map[string]string) (*Stream, error) {
	ctx, cancel := context.WithCancel(withMD(ctx, md))
	cs, err := c.cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, fullMethod)
	if err != nil {
		cancel()
		return nil, err
	}
	return &Stream{cs: cs, cancel: cancel}, nil
}

// Send sends one message.
func (s *Stream) Send(m proto.Message) error { return s.cs.SendMsg(m) }

// CloseSend ends the client's side.
func (s *Stream) CloseSend() error { return s.cs.CloseSend() }

// Recv receives one message: io.EOF when the server ended the stream with OK, its status otherwise.
func (s *Stream) Recv(m proto.Message) error { return s.cs.RecvMsg(m) }

// Close abandons the call.
func (s *Stream) Close() { s.cancel() }
