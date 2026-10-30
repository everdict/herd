package agent_test

import (
	"sort"
	"testing"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/internal/grpcwire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Every method of env's ProcessService and FileSystemService, and of herd's FileInfoService, is served under the
// full name its descriptor gives it, and nothing else is.
func TestServesEveryMethodUnderItsWireName(t *testing.T) {
	g, err := agent.New(newTarget(t, "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	srv := grpcwire.NewServer()
	g.Register(srv)
	var want []string
	for _, sd := range []protoreflect.ServiceDescriptor{
		ateenv.File_ateenv_v1alpha_guest_proto.Services().ByName("ProcessService"),
		ateenv.File_ateenv_v1alpha_guest_proto.Services().ByName("FileSystemService"),
		herdext.File_herdext_v1_files_proto.Services().ByName("FileInfoService"),
	} {
		for i := 0; i < sd.Methods().Len(); i++ {
			want = append(want, "/"+string(sd.FullName())+"/"+string(sd.Methods().Get(i).Name()))
		}
	}
	got := srv.Methods()
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
