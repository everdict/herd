package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/everdict/herd/api/ateapi/v1"
	_ "github.com/everdict/herd/api/herdext/v1"
)

// upstreamCommit is the Substrate commit the subset is cut from (spec herd.md: ateapi.proto @6d7b3f49).
const upstreamCommit = "6d7b3f49f6154d52a1de46c94a1481868a5ed042"

type rpc struct{ in, out string }

// controlSubset is the pinned subset of substrate.ateapi.v1.Control herd serves, with the request and response
// types upstream declares for each method at upstreamCommit.
var controlSubset = map[string]rpc{
	"CreateActor":             {"CreateActorRequest", "Actor"},
	"GetActor":                {"GetActorRequest", "Actor"},
	"ListActors":              {"ListActorsRequest", "ListActorsResponse"},
	"ListWorkers":             {"ListWorkersRequest", "ListWorkersResponse"},
	"UpdateActor":             {"UpdateActorRequest", "Actor"},
	"DeleteActor":             {"DeleteActorRequest", "Actor"},
	"SuspendActor":            {"SuspendActorRequest", "SuspendActorResponse"},
	"ResumeActor":             {"ResumeActorRequest", "ResumeActorResponse"},
	"CreateTag":               {"CreateTagRequest", "Tag"},
	"GetTag":                  {"GetTagRequest", "Tag"},
	"DeleteTag":               {"DeleteTagRequest", "Tag"},
	"CreateActorTemplate":     {"CreateActorTemplateRequest", "ActorTemplate"},
	"GetActorTemplate":        {"GetActorTemplateRequest", "ActorTemplate"},
	"DeleteActorTemplate":     {"DeleteActorTemplateRequest", "ActorTemplate"},
	"GetActorEgressPolicy":    {"GetActorEgressPolicyRequest", "EgressPolicy"},
	"CreateActorEgressPolicy": {"CreateActorEgressPolicyRequest", "EgressPolicy"},
	"UpdateActorEgressPolicy": {"UpdateActorEgressPolicyRequest", "EgressPolicy"},
	"DeleteActorEgressPolicy": {"DeleteActorEgressPolicyRequest", "EgressPolicy"},
}

// capabilitySubset is herd's own extension: what a worker declares it can do (spec R-D2, R-D9).
var capabilitySubset = map[string]rpc{
	"GetWorkerCapabilities":  {"GetWorkerCapabilitiesRequest", "WorkerCapabilities"},
	"ListWorkerCapabilities": {"ListWorkerCapabilitiesRequest", "ListWorkerCapabilitiesResponse"},
}

func service(t *testing.T, name protoreflect.FullName) protoreflect.ServiceDescriptor {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err != nil {
		t.Fatalf("service %s is not registered: %v", name, err)
	}
	s, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("%s is a %T, not a service", name, d)
	}
	return s
}

func checkExactly(t *testing.T, s protoreflect.ServiceDescriptor, want map[string]rpc) {
	t.Helper()
	pkg := s.ParentFile().Package()
	for name, r := range want {
		m := s.Methods().ByName(protoreflect.Name(name))
		if m == nil {
			t.Errorf("%s: method %s of the pinned subset is missing", s.FullName(), name)
			continue
		}
		if got := m.Input().FullName(); got != pkg.Append(protoreflect.Name(r.in)) {
			t.Errorf("%s.%s takes %s, want %s.%s", s.FullName(), name, got, pkg, r.in)
		}
		if got := m.Output().FullName(); got != pkg.Append(protoreflect.Name(r.out)) {
			t.Errorf("%s.%s returns %s, want %s.%s", s.FullName(), name, got, pkg, r.out)
		}
		if m.IsStreamingClient() || m.IsStreamingServer() {
			t.Errorf("%s.%s streams; the subset is unary", s.FullName(), name)
		}
	}
	for i := 0; i < s.Methods().Len(); i++ {
		if name := string(s.Methods().Get(i).Name()); want[name] == (rpc{}) {
			t.Errorf("%s: method %s is outside the pinned subset", s.FullName(), name)
		}
	}
}

// The Control subset keeps Substrate's package name so that full method names (/substrate.ateapi.v1.Control/...) are
// wire-identical to real Substrate.
func TestControlSubsetIsPinned(t *testing.T) {
	s := service(t, "substrate.ateapi.v1.Control")
	checkExactly(t, s, controlSubset)
}

func TestCapabilityExtension(t *testing.T) {
	s := service(t, "herd.ext.v1.Capabilities")
	checkExactly(t, s, capabilitySubset)
	d, err := protoregistry.GlobalFiles.FindDescriptorByName("herd.ext.v1.WorkerCapabilities")
	if err != nil {
		t.Fatalf("WorkerCapabilities: %v", err)
	}
	fields := d.(protoreflect.MessageDescriptor).Fields()
	if f := fields.ByName("suspend_max_fidelity"); f == nil || f.Enum() == nil || f.Enum().FullName() != "substrate.ateapi.v1.SnapshotFidelity" {
		t.Errorf("WorkerCapabilities.suspend_max_fidelity must be a substrate.ateapi.v1.SnapshotFidelity")
	}
	for _, name := range []protoreflect.Name{"restore_family", "portable_restore_family", "platform", "arch"} {
		if fields.ByName(name) == nil {
			t.Errorf("WorkerCapabilities lacks %s", name)
		}
	}
}

// The vendored subset names its source and is a cut of the pinned upstream file, not a rewrite of it.
func TestVendoredSubsetAttribution(t *testing.T) {
	proto, err := os.ReadFile("ateapi/v1/ateapi.proto")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Copyright 2026 Google LLC", "Apache License, Version 2.0", "github.com/agent-substrate/substrate", upstreamCommit} {
		if !strings.Contains(string(proto), w) {
			t.Errorf("ateapi.proto does not carry %q", w)
		}
	}
	upstream, err := os.ReadFile("ateapi/v1/testdata/upstream-ateapi.proto")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(upstream)
	upstreamMD, err := os.ReadFile("ateapi/v1/UPSTREAM.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upstreamMD), hex.EncodeToString(sum[:])) || !strings.Contains(string(upstreamMD), upstreamCommit) {
		t.Errorf("UPSTREAM.md does not pin the upstream file (commit %s, sha256 %x)", upstreamCommit, sum)
	}
}

// herd's one addition to the Control subset (PLT-330, herd-additions.txt): CreateActorTemplateRequest.image_pull_auth,
// a string numbered away from upstream's fields, registered as the generated code describes it. No message a read
// returns (ActorTemplate) carries it.
func TestHerdAdditionImagePullAuth(t *testing.T) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName("substrate.ateapi.v1.CreateActorTemplateRequest")
	if err != nil {
		t.Fatal(err)
	}
	f := d.(protoreflect.MessageDescriptor).Fields().ByName("image_pull_auth")
	if f == nil || f.Number() != 1000 || f.Kind() != protoreflect.StringKind || f.Cardinality() != protoreflect.Optional || f.JSONName() != "imagePullAuth" {
		t.Fatalf("CreateActorTemplateRequest.image_pull_auth = %v, want string image_pull_auth = 1000", f)
	}
	var walk func(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool)
	walk = func(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[m.FullName()] {
			return
		}
		seen[m.FullName()] = true
		for i := 0; i < m.Fields().Len(); i++ {
			fd := m.Fields().Get(i)
			if fd.Number() >= 1000 {
				t.Errorf("%s.%s: a herd addition in what a read returns", m.FullName(), fd.Name())
			}
			if fd.Message() != nil {
				walk(fd.Message(), seen)
			}
		}
	}
	tpl, err := protoregistry.GlobalFiles.FindDescriptorByName("substrate.ateapi.v1.ActorTemplate")
	if err != nil {
		t.Fatal(err)
	}
	walk(tpl.(protoreflect.MessageDescriptor), map[protoreflect.FullName]bool{})
}

// TestHerdAdditionsInTheDescriptor holds ateapi.pb.go to herd-additions.txt and ateapi.proto: every field the
// additions declare is in ateapi.proto and in the registered descriptor with the same number, type and label, and the
// descriptor's CreateActorTemplateRequest has no herd field they do not declare. DEFAUL-1600's
// image_plain_http_hosts was written into ateapi.pb.go without protoc (none in that box), its raw descriptor by
// descriptorpb and proto.Marshal; go generate ./api with protoc replaces it, and this test holds both to the same.
func TestHerdAdditionsInTheDescriptor(t *testing.T) {
	add, err := os.ReadFile("ateapi/v1/herd-additions.txt")
	if err != nil {
		t.Fatal(err)
	}
	proto, err := os.ReadFile("ateapi/v1/ateapi.proto")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]protoreflect.Kind{"string": protoreflect.StringKind}
	field := regexp.MustCompile(`(?m)^\s+(repeated )?(\w+) (\w+) = (\d+);$`)
	msgs := regexp.MustCompile(`(?s)message (\w+) \{(.*?)\n\}`).FindAllStringSubmatch(string(add), -1)
	if len(msgs) == 0 {
		t.Fatal("herd-additions.txt declares no message")
	}
	for _, m := range msgs {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName("substrate.ateapi.v1." + m[1]))
		if err != nil {
			t.Fatal(err)
		}
		md := d.(protoreflect.MessageDescriptor)
		declared := map[protoreflect.Name]bool{}
		for _, f := range field.FindAllStringSubmatch(m[2], -1) {
			if !strings.Contains(string(proto), f[0]+"\n") {
				t.Errorf("%s.%s: herd-additions.txt declares it, ateapi.proto does not (go generate ./api)", m[1], f[3])
			}
			declared[protoreflect.Name(f[3])] = true
			fd := md.Fields().ByName(protoreflect.Name(f[3]))
			card := protoreflect.Optional
			if f[1] != "" {
				card = protoreflect.Repeated
			}
			kind, ok := kinds[f[2]]
			if !ok {
				t.Fatalf("%s.%s: type %s: teach this test its kind", m[1], f[3], f[2])
			}
			if fd == nil || fmt.Sprint(fd.Number()) != f[4] || fd.Kind() != kind || fd.Cardinality() != card {
				t.Errorf("%s.%s: the descriptor has %v, herd-additions.txt declares %q", m[1], f[3], fd, strings.TrimSpace(f[0]))
			}
		}
		for i := 0; i < md.Fields().Len(); i++ {
			if fd := md.Fields().Get(i); fd.Number() >= 1000 && !declared[fd.Name()] {
				t.Errorf("%s.%s: a herd field herd-additions.txt does not declare", m[1], fd.Name())
			}
		}
	}
}
