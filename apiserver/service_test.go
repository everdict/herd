package apiserver_test

import (
	"sort"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/apiserver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The hand-written service descriptors serve exactly the methods the protos declare, under their full names, and
// every one is unary as in the proto.
func TestServiceDescsMatchProto(t *testing.T) {
	for _, c := range []struct {
		desc  *grpc.ServiceDesc
		proto protoreflect.ServiceDescriptor
	}{
		{&apiserver.ControlServiceDesc, ateapi.File_ateapi_v1_ateapi_proto.Services().ByName("Control")},
		{&apiserver.CapabilitiesServiceDesc, herdext.File_herdext_v1_capabilities_proto.Services().ByName("Capabilities")},
	} {
		if c.desc.ServiceName != string(c.proto.FullName()) {
			t.Errorf("service %q, proto %q", c.desc.ServiceName, c.proto.FullName())
		}
		var want, got []string
		for i := 0; i < c.proto.Methods().Len(); i++ {
			m := c.proto.Methods().Get(i)
			if m.IsStreamingClient() || m.IsStreamingServer() {
				t.Errorf("%s is streaming in the proto; the descriptor serves it unary", m.FullName())
			}
			want = append(want, string(m.Name()))
		}
		for _, m := range c.desc.Methods {
			got = append(got, m.MethodName)
		}
		if len(c.desc.Streams) != 0 {
			t.Errorf("%s: unexpected streams %v", c.desc.ServiceName, c.desc.Streams)
		}
		sort.Strings(want)
		sort.Strings(got)
		if len(got) != len(want) {
			t.Fatalf("%s serves %v, proto declares %v", c.desc.ServiceName, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s serves %v, proto declares %v", c.desc.ServiceName, got, want)
			}
		}
	}
}
