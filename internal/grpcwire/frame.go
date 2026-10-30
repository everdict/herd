package grpcwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxMessageSize bounds one received message (the length prefix is attacker-controlled).
const MaxMessageSize = 16 << 20

// writeFrame writes one length-prefixed, uncompressed message.
func writeFrame(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return Errorf(Internal, "marshal: %v", err)
	}
	if len(b) > MaxMessageSize {
		return Errorf(ResourceExhausted, "message of %d bytes exceeds %d", len(b), MaxMessageSize)
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
	if _, err := w.Write(append(hdr[:], b...)); err != nil {
		return err
	}
	return nil
}

// readFrame reads one message; io.EOF means the stream ended cleanly between messages.
func readFrame(r io.Reader, m proto.Message) error {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return Errorf(Internal, "reading message header: %v", err)
	}
	if hdr[0] != 0 {
		return Errorf(Unimplemented, "compressed messages are not supported")
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxMessageSize {
		return Errorf(ResourceExhausted, "message of %d bytes exceeds %d", n, MaxMessageSize)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return Errorf(Internal, "reading message body: %v", err)
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return Errorf(Internal, "unmarshal: %v", fmt.Sprint(err))
	}
	return nil
}
