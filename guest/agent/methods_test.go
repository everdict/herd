package agent_test

import (
	"fmt"
	"sort"
	"testing"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Every method of env's ProcessService and FileSystemService, and of herd's FileInfoService, is served under the
// full name its descriptor gives it, with the streaming kind it declares, and nothing else is.
func TestServesEveryMethodUnderItsWireName(t *testing.T) {
	g, err := agent.New(newTarget(t, "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	srv := grpc.NewServer()
	g.Register(srv)
	var want []string
	for _, sd := range []protoreflect.ServiceDescriptor{
		ateenv.File_ateenv_v1alpha_guest_proto.Services().ByName("ProcessService"),
		ateenv.File_ateenv_v1alpha_guest_proto.Services().ByName("FileSystemService"),
		herdext.File_herdext_v1_files_proto.Services().ByName("FileInfoService"),
	} {
		for i := 0; i < sd.Methods().Len(); i++ {
			m := sd.Methods().Get(i)
			want = append(want, fmt.Sprintf("/%s/%s client-stream=%v server-stream=%v", sd.FullName(), m.Name(), m.IsStreamingClient(), m.IsStreamingServer()))
		}
	}
	var got []string
	for name, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			got = append(got, fmt.Sprintf("/%s/%s client-stream=%v server-stream=%v", name, m.Name, m.IsClientStream, m.IsServerStream))
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("served %v,\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("served %v,\nwant %v", got, want)
		}
	}
}
