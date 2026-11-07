package conformance_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc/codes"
)

// DEFAUL-1611, herd-api: a template container's security_context.capabilities (add and drop, as Kubernetes writes
// them) reach the herdlet in the boot's link.Template; a container with neither field carries none; a name Linux
// does not have, or ALL in add, is refused at create with InvalidArgument naming it, and nothing is stored.
func TestTemplateCapabilitiesReachTheHerdlet(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Caps: chCaps(famA)})
	var mu sync.Mutex
	var boots []*link.Template
	e.Herdlets["h1"].Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op == link.OpBoot && w.Spec != nil {
			mu.Lock()
			boots = append(boots, w.Spec)
			mu.Unlock()
		}
		return o, true
	})
	create := func(name string, c *ateapi.Capabilities) error {
		var sc *ateapi.SecurityContext
		if c != nil {
			sc = &ateapi.SecurityContext{Capabilities: c}
		}
		_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
			Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
			Containers: []*ateapi.Container{
				{Name: "box", Image: "ghcr.io/everdict/agent:1", SecurityContext: sc},
				{Name: "plain", Image: "ghcr.io/everdict/agent:1"},
			},
			SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot},
		}})
		return err
	}

	if err := create("nonetraw", &ateapi.Capabilities{Add: []string{"SYS_PTRACE"}, Drop: []string{"NET_RAW", "CAP_MKNOD"}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	mkActor(t, e, "a1", "nonetraw", nil)
	mu.Lock()
	if len(boots) != 1 {
		mu.Unlock()
		t.Fatalf("%d boots reached the herdlet, want 1", len(boots))
	}
	box, plain := boots[0].Containers[0], boots[0].Containers[1]
	mu.Unlock()
	if !slices.Equal(box.CapAdd, []string{"SYS_PTRACE"}) || !slices.Equal(box.CapDrop, []string{"NET_RAW", "CAP_MKNOD"}) {
		t.Errorf("boot carries add %q drop %q for box, want the template's add SYS_PTRACE, drop NET_RAW CAP_MKNOD", box.CapAdd, box.CapDrop)
	}
	if plain.CapAdd != nil || plain.CapDrop != nil {
		t.Errorf("boot carries add %q drop %q for a container with no security_context, want none", plain.CapAdd, plain.CapDrop)
	}

	for _, tc := range []struct {
		name string
		caps *ateapi.Capabilities
		bad  string
	}{
		{"unknown-drop", &ateapi.Capabilities{Drop: []string{"NET_RAWR"}}, "NET_RAWR"},
		{"unknown-add", &ateapi.Capabilities{Add: []string{"CAP_SYS_PTRACE", "CAP_TELEPORT"}}, "CAP_TELEPORT"},
		{"add-all", &ateapi.Capabilities{Add: []string{"ALL"}}, "ALL"},
	} {
		err := create(tc.name, tc.caps)
		wantCode(t, "CreateActorTemplate "+tc.name, err, codes.InvalidArgument)
		if err == nil || !strings.Contains(err.Error(), tc.bad) || !strings.Contains(err.Error(), "box") {
			t.Errorf("CreateActorTemplate %s: %v, want a refusal naming container box and %s", tc.name, err, tc.bad)
		}
		_, err = e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, tc.name)})
		wantCode(t, "GetActorTemplate "+tc.name, err, codes.NotFound)
	}
}
