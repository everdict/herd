package s3_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore/s3"
)

// TestMetadataCallsEndAtTheirContextOnAFrozenEndpoint (PLT-375): against an endpoint that takes the connection and
// never answers, as MinIO frozen with docker pause does, Stat and Delete end at their context's deadline, so the bound
// herd-api gives them (apiserver storeCall) is the bound its callers see. Not a counterexample: the store already
// honoured its context; this holds that the bound reaches the wire.
func TestMetadataCallsEndAtTheirContextOnAFrozenEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	s, err := s3.New(s3.Config{Endpoint: "http://" + ln.Addr().String(), Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const bound = 300 * time.Millisecond
	for _, c := range []struct {
		name string
		call func(context.Context) error
	}{
		{"Stat", func(ctx context.Context) error { _, err := s.Stat(ctx, "k"); return err }},
		{"Delete", func(ctx context.Context) error { return s.Delete(ctx, "k") }},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), bound)
		start := time.Now()
		err := c.call(ctx)
		cancel()
		if took := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || took > bound+time.Second {
			t.Errorf("%s on a frozen endpoint = %v after %v; want the context's deadline within %v", c.name, err, took, bound)
		}
	}
}
