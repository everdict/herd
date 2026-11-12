package conformance_test

import (
	"context"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
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

// method is one RPC herd-api's server registered: a proto method (md set), or a method of a hand-registered JSON
// service (json is its request).
type method struct {
	full string
	md   protoreflect.MethodDescriptor
	json any
}

// jsonRequests are the requests of the hand-registered JSON services' unary methods, which the matrix calls.
func jsonRequests(space, name, worker string) map[string]any {
	return map[string]any{
		apiserver.CheckActorWriteMethod: &apiserver.CheckActorWriteRequest{Atespace: space, Name: name},
		apiserver.WorkerIdentityMethod:  &apiserver.WorkerIdentityRequest{Worker: worker},
	}
}

// notInMatrix are the registered methods the role matrix does not call, each with why and where it is tested. Adding
// a method to herd-api means adding it to the matrix or here.
var notInMatrix = map[string]string{
	"/" + link.ServiceName + "/" + link.StreamName: "the herdlet link, a bidirectional stream only a herdlet credential opens " +
		"(apiserver serveLink: p.Herdlet); its refusals are deploy.TestJoinRefusesWhatIsNotItsNode (a join token), " +
		"conformance.TestHS256CannotTakeAnSVIDWorker, and its registration checks the link tests",
}

// methods is every RPC herd-api's real server registered (grpc.Server.GetServiceInfo): the population, hand-registered
// services included, so a method added later is in the test or in notInMatrix, never in neither.
func methods(t *testing.T, e *conformance.Env) []method {
	t.Helper()
	var out []method
	var missing []string
	reqs := jsonRequests("", "", "")
	seen := 0
	for svc, info := range e.ServiceInfo() {
		d, _ := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
		sd, _ := d.(protoreflect.ServiceDescriptor)
		for _, mi := range info.Methods {
			seen++
			full := "/" + svc + "/" + mi.Name
			switch {
			case notInMatrix[full] != "":
			case sd != nil && sd.Methods().ByName(protoreflect.Name(mi.Name)) != nil && !mi.IsClientStream && !mi.IsServerStream:
				out = append(out, method{full: full, md: sd.Methods().ByName(protoreflect.Name(mi.Name))})
			case reqs[full] != nil:
				out = append(out, method{full: full, json: reqs[full]})
			default:
				missing = append(missing, full)
			}
		}
	}
	if seen < 23 {
		t.Fatalf("herd-api's server registered %d methods; the Control subset alone has 20", seen)
	}
	if len(missing) > 0 {
		t.Fatalf("registered methods neither the role matrix calls nor notInMatrix lists with its reason: %v", missing)
	}
	return out
}

func call(ctx context.Context, t *testing.T, c *conformance.Client, m method, space, name, worker string) error {
	t.Helper()
	if m.md == nil {
		var resp map[string]any
		return c.Conn().Invoke(ctx, m.full, jsonRequests(space, name, worker)[m.full], &resp, grpc.CallContentSubtype(link.CodecName))
	}
	md := m.md
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
	allowed := map[string]map[string]bool{
		"spiffe://herd.test/herd/egress/w1": {"/substrate.ateapi.v1.Control/GetActorEgressPolicy": true},
		"spiffe://herd.test/herd/node/w1": {"/substrate.ateapi.v1.Control/GetActorEgressPolicy": true,
			"/herd.ext.v1.Capabilities/GetWorkerCapabilities": true},
	}
	for id, ok := range allowed {
		c := svidClient(t, e, ca, id)
		for _, m := range methods(t, e) {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := call(cctx, t, c, m, e.Space, "a1", "w1")
			cancel()
			denied := status.Code(err) == codes.PermissionDenied
			if ok[m.full] == denied {
				t.Errorf("%s calling %s: %v; want it %s", id, m.full, err, map[bool]string{true: "allowed", false: "refused PermissionDenied"}[ok[m.full]])
			}
		}
	}
	// A node reads its own capabilities only.
	node := svidClient(t, e, ca, "spiffe://herd.test/herd/node/w2")
	for _, m := range methods(t, e) {
		if m.full == "/herd.ext.v1.Capabilities/GetWorkerCapabilities" {
			if err := call(ctx, t, node, m, e.Space, "a1", "w1"); status.Code(err) != codes.PermissionDenied {
				t.Errorf("node w2 reading w1's capabilities: %v; want PermissionDenied", err)
			}
		}
	}
}
