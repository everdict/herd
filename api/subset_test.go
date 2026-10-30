package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
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
	"CreateActor":               {"CreateActorRequest", "Actor"},
	"GetActor":                  {"GetActorRequest", "Actor"},
	"ListActors":                {"ListActorsRequest", "ListActorsResponse"},
	"UpdateActor":               {"UpdateActorRequest", "Actor"},
	"DeleteActor":               {"DeleteActorRequest", "Actor"},
	"SuspendActor":              {"SuspendActorRequest", "SuspendActorResponse"},
	"ResumeActor":               {"ResumeActorRequest", "ResumeActorResponse"},
	"CreateTag":                 {"CreateTagRequest", "Tag"},
	"GetTag":                    {"GetTagRequest", "Tag"},
	"DeleteTag":                 {"DeleteTagRequest", "Tag"},
	"CreateActorTemplate":       {"CreateActorTemplateRequest", "ActorTemplate"},
	"GetActorTemplate":          {"GetActorTemplateRequest", "ActorTemplate"},
	"DeleteActorTemplate":       {"DeleteActorTemplateRequest", "ActorTemplate"},
	"GetActorEgressPolicy":      {"GetActorEgressPolicyRequest", "EgressPolicy"},
	"CreateActorEgressPolicy":   {"CreateActorEgressPolicyRequest", "EgressPolicy"},
	"UpdateActorEgressPolicy":   {"UpdateActorEgressPolicyRequest", "EgressPolicy"},
	"DeleteActorEgressPolicy":   {"DeleteActorEgressPolicyRequest", "EgressPolicy"},
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
