package conformance_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"google.golang.org/grpc"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

// TestHerdletDialsOut: herd-api learns of a herdlet only when the herdlet connects; it registers capabilities and
// capacity on the stream, and the Capabilities extension reports them.
func TestHerdletDialsOut(t *testing.T) {
	e := conformance.Start(t)
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	_, err := e.Client.CreateActor(ctx, actorReq(e, "a1", "tpl", nil))
	wantCode(t, "CreateActor with no herdlet connected", err, codes.ResourceExhausted)

	e.AddHerdlet(t, conformance.HerdletSpec{Name: "h1", Slots: 2, Caps: chCaps(famA)})
	mkActor(t, e, "a1", "tpl", nil)

	ro := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false))
	defer ro.Close()
	c := ro.Conn()
	var caps herdext.WorkerCapabilities
	if err := c.Invoke(ctx, "/herd.ext.v1.Capabilities/GetWorkerCapabilities", &herdext.GetWorkerCapabilitiesRequest{Worker: "h1"}, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.GetPlatform() != herdext.Platform_PLATFORM_LINUX_CLOUD_HYPERVISOR || caps.GetRestoreFamily() != famA ||
		caps.GetSuspendMaxFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || caps.GetPortableRestoreFamily() != vmm.PortableRestoreFamily {
		t.Errorf("capabilities = %v", &caps)
	}
	var list herdext.ListWorkerCapabilitiesResponse
	if err := c.Invoke(ctx, "/herd.ext.v1.Capabilities/ListWorkerCapabilities", &herdext.ListWorkerCapabilitiesRequest{}, &list); err != nil || len(list.GetWorkers()) != 1 {
		t.Errorf("ListWorkerCapabilities = %v, %v", &list, err)
	}

	// Only a herdlet credential may open the link.
	err = register(ctx, e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite}, false), `{"register":{"worker":"rogue","class":"microvm","capacity":{"slots":99}}}`)
	wantCode(t, "a client credential opening the herdlet link", err, codes.PermissionDenied)

	// A herdlet credential registers only the worker it names.
	err = register(ctx, e.Addr, e.TokenFor(t, "h1", nil, true), `{"register":{"worker":"h2","class":"microvm","capacity":{"slots":99}}}`)
	wantCode(t, "herdlet h1 registering as h2", err, codes.PermissionDenied)
}

// TestHerdletTimeoutIsNotSuccess: a boot the herdlet does not answer in time, or answers unknown, leaves the actor
// RESUMING with the boot owed — never RUNNING — until the herdlet's answer settles it.
func TestHerdletTimeoutIsNotSuccess(t *testing.T) {
	for _, c := range []struct {
		name   string
		inject func(h *conformance.Herdlet)
	}{
		{"hung hypervisor", func(h *conformance.Herdlet) { h.VMM.Hang(vmm.OpBoot) }},
		{"unreachable hypervisor", func(h *conformance.Herdlet) { h.VMM.Unreachable(vmm.OpBoot) }},
		{"silent herdlet", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) { return o, w.Op != link.OpBoot })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
			ctx := ctxT(t)
			mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
			h := e.Herdlets["h1"]
			c.inject(h)
			_, err := e.Client.CreateActor(ctx, actorReq(e, "a1", "tpl", nil))
			wantCode(t, "CreateActor", err, codes.Unavailable)
			got, owed := getActor(t, e, "a1")
			if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RESUMING || !strings.HasPrefix(owed, "boot: ") {
				t.Errorf("state %v owed %q, want RESUMING with the boot owed", got.GetStatus().GetState(), owed)
			}
			h.VMM.Clear()
			h.Rewrite(nil)
			if left, err := e.Server.Reconcile(ctx); err != nil || left != 0 {
				t.Fatalf("Reconcile = %d, %v", left, err)
			}
			got, owed = getActor(t, e, "a1")
			if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || owed != "" || present(t, h, got) != vmm.PresencePresent {
				t.Errorf("after reconcile: %v owed %q", got.GetStatus().GetState(), owed)
			}
		})
	}
}

// TestHerdletGoneIsUnknown: work for a herdlet whose link dropped is unknown, not done.
func TestHerdletGoneIsUnknown(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	e.Herdlets["h1"].Disconnect()
	if o := e.Hub.Call(context.Background(), "h1", link.Work{Op: link.OpInspect, VM: "x"}); o.Result != link.ResultUnknown {
		t.Errorf("call to a disconnected herdlet = %+v, want unknown", o)
	}
	_, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "DeleteActor with the herdlet gone", err, codes.Unavailable)
	if got, owed := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_DELETING || !strings.Contains(owed, "not connected") {
		t.Errorf("state %v owed %q", got.GetStatus().GetState(), owed)
	}
}

// TestHerdAPINeverDials: herd-api's packages hold no dialer — no net.Dial*, no grpc.Dial or grpc.NewClient, no http client.
// herd-api's main is deploy.API (deploy/api.go) and its join service package join.
func TestHerdAPINeverDials(t *testing.T) {
	fset := token.NewFileSet()
	for _, dir := range []string{"../apiserver", "../link", "../join", "../cmd/herd-api", "../deploy/api.go"} {
		pkgs, err := parseSources(fset, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			for name, f := range p.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if x, ok := sel.X.(*ast.Ident); ok {
						call := x.Name + "." + sel.Sel.Name
						if strings.HasPrefix(sel.Sel.Name, "Dial") || call == "grpc.NewClient" || call == "http.Client" || call == "http.Get" || call == "http.Post" {
							t.Errorf("%s: %s at %v — herd-api must not dial out", filepath.Base(name), call, fset.Position(sel.Pos()))
						}
					}
					return true
				})
			}
		}
	}
}

// register opens the herdlet link with token, sends one registration and returns how the server answered it.
func register(ctx context.Context, addr, token, reg string) error {
	cl := conformance.NewClient(addr, token)
	defer cl.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cl.Conn().NewStream(ctx, &link.StreamDesc, link.LinkMethod, grpc.CallContentSubtype(link.CodecName))
	if err != nil {
		return err
	}
	st := link.NewGRPCStream(cs)
	if err := st.Send([]byte(reg)); err != nil {
		return err
	}
	_, err = st.Recv()
	return err
}

// parseSources parses the non-test Go files of a directory, or one Go file.
func parseSources(fset *token.FileSet, path string) (map[string]*ast.Package, error) {
	if strings.HasSuffix(path, ".go") {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		return map[string]*ast.Package{f.Name.Name: {Name: f.Name.Name, Files: map[string]*ast.File{path: f}}}, nil
	}
	return parser.ParseDir(fset, path, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
}
