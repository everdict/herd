// Package grpcwire speaks the gRPC wire protocol (HTTP/2 POST, length-prefixed protobuf messages, grpc-status in
// trailers) over Go's standard net/http with unencrypted HTTP/2, so herd-guest serves agent-substrate/env's
// ProcessService and FileSystemService to any gRPC client without the grpc-go module. It implements what
// herd-guest needs: unary, server-streaming and client-streaming calls, metadata, grpc-timeout and status codes.
// It does not implement compression, reflection or keepalive policy.
package grpcwire

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Code is a gRPC status code (google.rpc.Code numbers).
type Code uint32

const (
	OK                 Code = 0
	Canceled           Code = 1
	Unknown            Code = 2
	InvalidArgument    Code = 3
	DeadlineExceeded   Code = 4
	NotFound           Code = 5
	AlreadyExists      Code = 6
	PermissionDenied   Code = 7
	ResourceExhausted  Code = 8
	FailedPrecondition Code = 9
	Aborted            Code = 10
	OutOfRange         Code = 11
	Unimplemented      Code = 12
	Internal           Code = 13
	Unavailable        Code = 14
	DataLoss           Code = 15
	Unauthenticated    Code = 16
)

// Status is a call's outcome other than OK. It is an error.
type Status struct {
	Code    Code
	Message string
}

func (s *Status) Error() string {
	return fmt.Sprintf("rpc error: code = %d desc = %s", s.Code, s.Message)
}

// Errorf returns a *Status error.
func Errorf(code Code, format string, args ...any) error {
	return &Status{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the status code err carries: OK for nil, Canceled or DeadlineExceeded for context errors, Unknown
// for anything else that is not a *Status.
func CodeOf(err error) Code {
	if err == nil {
		return OK
	}
	var s *Status
	switch {
	case errors.As(err, &s):
		return s.Code
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return DeadlineExceeded
	}
	return Unknown
}

func statusOf(err error) (Code, string) {
	if err == nil {
		return OK, ""
	}
	var s *Status
	if errors.As(err, &s) {
		return s.Code, s.Message
	}
	return CodeOf(err), err.Error()
}

// encodeMessage percent-encodes a grpc-message value as the gRPC HTTP/2 spec asks.
func encodeMessage(msg string) string {
	var b []byte
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c >= 0x20 && c <= 0x7e && c != '%' {
			b = append(b, c)
			continue
		}
		b = append(b, fmt.Sprintf("%%%02X", c)...)
	}
	return string(b)
}

func decodeMessage(v string) string {
	if s, err := url.PathUnescape(v); err == nil {
		return s
	}
	return v
}

func parseStatus(code, msg string) error {
	if code == "" {
		return Errorf(Internal, "server sent no grpc-status")
	}
	n, err := strconv.ParseUint(code, 10, 32)
	if err != nil {
		return Errorf(Internal, "malformed grpc-status %q", code)
	}
	if Code(n) == OK {
		return nil
	}
	return &Status{Code: Code(n), Message: decodeMessage(msg)}
}
