package api_test

import (
	"os"
	"regexp"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	herdext "github.com/everdict/herd/api/herdext/v1"
)

// TestClockProtoDescriptor holds herdext/v1/clock.pb.go, written by hand in protoc-gen-go's shape (no protoc where
// DEFAUL-1595 was written), to clock.proto: every message, field and rpc the .proto declares is in the registered
// descriptor with the same number and types, and nothing more, and a message survives the wire.
func TestClockProtoDescriptor(t *testing.T) {
	src, err := os.ReadFile("herdext/v1/clock.proto")
	if err != nil {
		t.Fatal(err)
	}
	fd := herdext.File_herdext_v1_clock_proto
	if fd.Path() != "herdext/v1/clock.proto" || fd.Package() != "herd.ext.v1" || fd.Syntax() != protoreflect.Proto3 {
		t.Fatalf("descriptor %s package %s syntax %v", fd.Path(), fd.Package(), fd.Syntax())
	}
	msgs := regexp.MustCompile(`(?s)message (\w+) \{(.*?)\n\}`).FindAllStringSubmatch(string(src), -1)
	if len(msgs) != fd.Messages().Len() {
		t.Fatalf("clock.proto declares %d messages, the descriptor has %d", len(msgs), fd.Messages().Len())
	}
	field := regexp.MustCompile(`\n\s+([\w.]+) (\w+) = (\d+);`)
	for _, m := range msgs {
		md := fd.Messages().ByName(protoreflect.Name(m[1]))
		if md == nil {
			t.Fatalf("message %s is not in the descriptor", m[1])
		}
		fs := field.FindAllStringSubmatch(m[2], -1)
		if len(fs) != md.Fields().Len() {
			t.Fatalf("%s: clock.proto declares %d fields, the descriptor has %d", m[1], len(fs), md.Fields().Len())
		}
		for _, f := range fs {
			d := md.Fields().ByName(protoreflect.Name(f[2]))
			if d == nil || string(d.Message().FullName()) != f[1] || f[3] != "1" || d.Number() != 1 {
				t.Fatalf("%s.%s: the descriptor has %v", m[1], f[2], d)
			}
		}
	}
	rpcs := regexp.MustCompile(`rpc (\w+)\((\w+)\) returns \((\w+)\);`).FindAllStringSubmatch(string(src), -1)
	sd := fd.Services().ByName("ClockService")
	if sd == nil || fd.Services().Len() != 1 || len(rpcs) != sd.Methods().Len() {
		t.Fatalf("service ClockService: descriptor %v, clock.proto declares %d rpcs", sd, len(rpcs))
	}
	for _, r := range rpcs {
		m := sd.Methods().ByName(protoreflect.Name(r[1]))
		if m == nil || string(m.Input().Name()) != r[2] || string(m.Output().Name()) != r[3] || m.IsStreamingClient() || m.IsStreamingServer() {
			t.Fatalf("rpc %s: the descriptor has %v", r[1], m)
		}
	}
	now := time.Date(2026, 11, 6, 7, 44, 42, 5, time.UTC)
	b, err := proto.Marshal(&herdext.SetClockRequest{Now: timestamppb.New(now)})
	if err != nil {
		t.Fatal(err)
	}
	var got herdext.SetClockRequest
	if err := proto.Unmarshal(b, &got); err != nil || !got.GetNow().AsTime().Equal(now) {
		t.Fatalf("round trip: %v, %v", got.GetNow(), err)
	}
}
