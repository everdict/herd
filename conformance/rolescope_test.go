package conformance_test

import (
	"context"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/conformance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// fill names space and name in every atespace/name field of m, and in its messages' (an ObjectRef, a
// ResourceMetadata), two levels down, so a request reaches its authorization rather than its argument checks.
func fill(m protoreflect.Message, space, name, worker string, depth int) {
	fs := m.Descriptor().Fields()
	for i := 0; i < fs.Len(); i++ {
		f := fs.Get(i)
		switch {
		case f.Kind() == protoreflect.StringKind && f.Cardinality() != protoreflect.Repeated:
			switch f.Name() {
			case "atespace":
				m.Set(f, protoreflect.ValueOfString(space))
			case "name":
				m.Set(f, protoreflect.ValueOfString(name))
			case "worker":
				m.Set(f, protoreflect.ValueOfString(worker))
			}
		case f.Kind() == protoreflect.MessageKind && f.Cardinality() != protoreflect.Repeated && !f.IsMap() && depth > 0:
			fill(m.Mutable(f).Message(), space, name, worker, depth-1)
		}
	}
}

// methods is every unary method herd-api serves, from its service descriptors: the population, so an RPC added later
// is in the test without anyone listing it.
func methods(t *testing.T) []protoreflect.MethodDescriptor {
	t.Helper()
	var out []protoreflect.MethodDescriptor
	for _, sd := range []grpc.ServiceDesc{apiserver.ControlServiceDesc, apiserver.CapabilitiesServiceDesc} {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(sd.ServiceName))
		if err != nil {
			t.Fatal(err)
		}
		svc := d.(protoreflect.ServiceDescriptor)
		for _, m := range sd.Methods {
			md := svc.Methods().ByName(protoreflect.Name(m.MethodName))
			if md == nil {
				t.Fatalf("%s/%s is not in the proto", sd.ServiceName, m.MethodName)
			}
			out = append(out, md)
		}
	}
	if len(out) < 20 {
		t.Fatalf("enumerated %d methods; the Control subset alone has 20", len(out))
	}
	return out
}

func call(ctx context.Context, t *testing.T, c *conformance.Client, md protoreflect.MethodDescriptor, space, name, worker string) error {
	t.Helper()
	in, err := protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
	if err != nil {
		t.Fatal(err)
	}
	out, err := protoregistry.GlobalTypes.FindMessageByName(md.Output().FullName())
	if err != nil {
		t.Fatal(err)
	}
	req := in.New()
	fill(req, space, name, worker, 2)
	return c.Conn().Invoke(ctx, "/"+string(md.Parent().FullName())+"/"+string(md.Name()), req.Interface(), out.New().Interface())
}

// TestEgressAndNodeRolesReachOnlyWhatTheyNeed (PLT-489 review 9): through the real server, over every RPC herd-api
// serves, herd-egress's identity and a node's are refused all but reading an actor's egress policy, and the node also
// its own capabilities; another worker's capabilities and the worker list are refused to both.
func TestEgressAndNodeRolesReachOnlyWhatTheyNeed(t *testing.T) {
	e, ca := spiffeEnv(t, conformance.HerdletSpec{Name: "w1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	allowed := map[string]map[protoreflect.Name]bool{
		"spiffe://herd.test/herd/egress/w1": {"GetActorEgressPolicy": true},
		"spiffe://herd.test/herd/node/w1":   {"GetActorEgressPolicy": true, "GetWorkerCapabilities": true},
	}
	for id, ok := range allowed {
		c := svidClient(t, e, ca, id)
		for _, md := range methods(t) {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := call(cctx, t, c, md, e.Space, "a1", "w1")
			cancel()
			denied := status.Code(err) == codes.PermissionDenied
			if ok[md.Name()] == denied {
				t.Errorf("%s calling %s: %v; want it %s", id, md.Name(), err, map[bool]string{true: "allowed", false: "refused PermissionDenied"}[ok[md.Name()]])
			}
		}
	}
	// A node reads its own capabilities only.
	node := svidClient(t, e, ca, "spiffe://herd.test/herd/node/w2")
	gw := methods(t)
	for _, md := range gw {
		if md.Name() == "GetWorkerCapabilities" {
			if err := call(ctx, t, node, md, e.Space, "a1", "w1"); status.Code(err) != codes.PermissionDenied {
				t.Errorf("node w2 reading w1's capabilities: %v; want PermissionDenied", err)
			}
		}
	}
}
