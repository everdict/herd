// Package tunnel is the herdlet's outbound tunnel to herd-router (spec R3; Substrate's atunnel). A herdlet dials the
// router and holds one control stream, /herd.ext.v1.Tunnel/Control; the router never dials a node. When the router
// needs a connection to a port of a VM on that node it sends Open on the control stream, and the herdlet connects to
// the guest (vsock on a real host) and opens a new outbound stream, /herd.ext.v1.Tunnel/Data, which carries that
// connection's bytes. A node behind NAT therefore needs no inbound port.
//
// A data stream ends in one of three ways: either side closes it, the link breaks, or the herdlet cuts it with a
// typed Reason (for example the actor is being suspended). A cut reason reaches the router before the stream ends,
// so the router can tell its client why, and the client can re-attach.
//
// The control stream's messages are JSON (link's codec); data streams use a small binary frame codec.
//
// This package is the protocol and the router's side (Server). The herdlet's side is in package herdlet.
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/encoding"
)

// gRPC names.
const (
	ServiceName   = "herd.ext.v1.Tunnel"
	ControlMethod = "/" + ServiceName + "/Control"
	DataMethod    = "/" + ServiceName + "/Data"
	// IDHeader is the data stream's request metadata naming the Open it answers.
	IDHeader = "herd-tunnel-id"
)

// Reason says why a tunnel could not be opened or was cut.
type Reason string

const (
	// ReasonSuspended: the actor is being suspended; its streams are cut and new ones refused until it resumes.
	ReasonSuspended Reason = "actor-suspended"
	// ReasonDeleted: the actor is being deleted.
	ReasonDeleted Reason = "actor-deleted"
	// ReasonGuestUnreachable: the herdlet could not connect to the port in the guest.
	ReasonGuestUnreachable Reason = "guest-unreachable"
	// ReasonNodeUnreachable: the node's herdlet has no tunnel to this router, or it broke.
	ReasonNodeUnreachable Reason = "node-unreachable"
	// ReasonOpenTimeout: the herdlet did not answer an Open in time.
	ReasonOpenTimeout Reason = "tunnel-open-timeout"
)

// Error is a typed tunnel failure.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "tunnel: " + string(e.Reason)
	}
	return fmt.Sprintf("tunnel: %s: %s", e.Reason, e.Detail)
}

// Up is a herdlet → router control message. Exactly one field is set.
type Up struct {
	Hello   *Hello   `json:"hello,omitempty"`
	Refused *Refused `json:"refused,omitempty"`
}

// Hello is the herdlet's first message: the worker it is.
type Hello struct {
	Worker string `json:"worker"`
}

// Refused answers an Open the herdlet could not carry out.
type Refused struct {
	ID     string `json:"id"`
	Reason Reason `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Down is a router → herdlet control message. Exactly one field is set.
type Down struct {
	Welcome *Welcome `json:"welcome,omitempty"`
	Open    *Open    `json:"open,omitempty"`
}

// Welcome acknowledges Hello.
type Welcome struct {
	Worker string `json:"worker"`
}

// Open asks the herdlet for a connection to port of VM, answered by a Data stream carrying IDHeader = ID.
type Open struct {
	ID   string `json:"id"`
	VM   string `json:"vm"`
	Port uint32 `json:"port"`
}

// FrameKind is what a data frame carries.
type FrameKind byte

const (
	// FrameData carries bytes.
	FrameData FrameKind = iota
	// FrameCut is the herdlet cutting the connection: Reason and Detail say why. Nothing follows it.
	FrameCut
	// FrameClose is the router closing its side: the herdlet closes the guest connection and ends the stream.
	FrameClose
)

// Frame is one message on a data stream.
type Frame struct {
	Kind   FrameKind
	Data   []byte
	Reason Reason
	Detail string
}

// FrameCodecName is the content-subtype of data streams (application/grpc+herdframe).
const FrameCodecName = "herdframe"

// FrameCodec encodes a *Frame as one kind byte followed by the data, or for a cut by a length-prefixed reason and the
// detail.
type FrameCodec struct{}

func (FrameCodec) Name() string { return FrameCodecName }

func (FrameCodec) Marshal(v any) ([]byte, error) {
	f, ok := v.(*Frame)
	if !ok {
		return nil, fmt.Errorf("tunnel: cannot encode %T as a frame", v)
	}
	switch f.Kind {
	case FrameData:
		return append([]byte{byte(FrameData)}, f.Data...), nil
	case FrameClose:
		return []byte{byte(FrameClose)}, nil
	case FrameCut:
		b := []byte{byte(FrameCut)}
		b = binary.AppendUvarint(b, uint64(len(f.Reason)))
		b = append(b, f.Reason...)
		return append(b, f.Detail...), nil
	}
	return nil, fmt.Errorf("tunnel: unknown frame kind %d", f.Kind)
}

var errBadFrame = errors.New("tunnel: malformed frame")

func (FrameCodec) Unmarshal(data []byte, v any) error {
	f, ok := v.(*Frame)
	if !ok {
		return fmt.Errorf("tunnel: cannot decode a frame into %T", v)
	}
	if len(data) == 0 {
		return errBadFrame
	}
	*f = Frame{Kind: FrameKind(data[0])}
	switch f.Kind {
	case FrameData:
		// The buffer is the transport's and may be reused once Unmarshal returns.
		f.Data = append([]byte(nil), data[1:]...)
	case FrameCut:
		n, k := binary.Uvarint(data[1:])
		if k <= 0 || uint64(len(data)-1-k) < n {
			return errBadFrame
		}
		rest := data[1+k:]
		f.Reason, f.Detail = Reason(rest[:n]), string(rest[n:])
	case FrameClose:
	default:
		return errBadFrame
	}
	return nil
}

func init() { encoding.RegisterCodec(FrameCodec{}) }

// validVM accepts what herd uses as VM ids (actor uids): a short label of letters, digits and dashes.
func validVM(vm string) bool {
	return vm != "" && len(vm) <= 128 && strings.Trim(vm, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") == ""
}
