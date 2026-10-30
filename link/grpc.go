package link

import (
	"encoding/json"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

// The link's gRPC names: LinkMethod is "/" + ServiceName + "/" + StreamName.
const (
	ServiceName = "herd.ext.v1.Herdlet"
	StreamName  = "Link"
)

// CodecName is the gRPC content-subtype the link's messages travel in (application/grpc+json).
const CodecName = "json"

// Codec is a gRPC codec for JSON messages; a json.RawMessage passes through unchanged.
type Codec struct{}

func (Codec) Name() string { return CodecName }

func (Codec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func (Codec) Unmarshal(data []byte, v any) error {
	if raw, ok := v.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	return json.Unmarshal(data, v)
}

func init() { encoding.RegisterCodec(Codec{}) }

// StreamDesc describes the link stream for a client's NewStream.
var StreamDesc = grpc.StreamDesc{StreamName: StreamName, ServerStreams: true, ClientStreams: true}

// grpcStream is a Stream over a grpc stream (either side).
type grpcStream struct {
	s interface {
		SendMsg(any) error
		RecvMsg(any) error
	}
}

// NewGRPCStream adapts a grpc.ServerStream or grpc.ClientStream opened with the json codec to a Stream.
func NewGRPCStream(s interface {
	SendMsg(any) error
	RecvMsg(any) error
}) Stream {
	return grpcStream{s}
}

func (g grpcStream) Send(b []byte) error {
	if !json.Valid(b) {
		return fmt.Errorf("link: not a JSON message")
	}
	return g.s.SendMsg(json.RawMessage(b))
}

func (g grpcStream) Recv() ([]byte, error) {
	var m json.RawMessage
	if err := g.s.RecvMsg(&m); err != nil {
		return nil, err
	}
	return m, nil
}
