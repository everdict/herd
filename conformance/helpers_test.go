package conformance_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	famA = "microvm-ch-amd64-tigerlake"
	famB = "microvm-ch-amd64-lunarlake"
)

func chCaps(fam string) vmm.Capabilities {
	return vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: fam}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mkTemplate(t *testing.T, e *conformance.Env, name string, f ateapi.SnapshotFidelity) *ateapi.ActorTemplate {
	t.Helper()
	tpl, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: f, GoldenSnapshotConfig: coldBoot},
	}})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return tpl
}

func actorReq(e *conformance.Env, name, tpl string, labels map[string]string) *ateapi.CreateActorRequest {
	a := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name}, ActorTemplate: &ateapi.ObjectRef{Atespace: e.Space, Name: tpl}}
	if labels != nil {
		a.WorkerSelector = &ateapi.Selector{MatchLabels: labels}
	}
	return &ateapi.CreateActorRequest{Actor: a}
}

func mkActor(t *testing.T, e *conformance.Env, name, tpl string, labels map[string]string) *ateapi.Actor {
	t.Helper()
	a, err := e.Client.CreateActor(ctxT(t), actorReq(e, name, tpl, labels))
	if err != nil {
		t.Fatalf("CreateActor %s: %v", name, err)
	}
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("CreateActor %s: state %v", name, a.GetStatus().GetState())
	}
	return a
}

func oref(e *conformance.Env, name string) *ateapi.ObjectRef {
	return &ateapi.ObjectRef{Atespace: e.Space, Name: name}
}

func getActor(t *testing.T, e *conformance.Env, name string) (*ateapi.Actor, string) {
	t.Helper()
	a, owed, err := e.Client.GetActorOwed(ctxT(t), e.Space, name)
	if err != nil {
		t.Fatalf("GetActor %s: %v", name, err)
	}
	return a, owed
}

func wantCode(t *testing.T, what string, err error, c codes.Code) {
	t.Helper()
	if got := status.Code(err); got != c {
		t.Errorf("%s: code %v (%v), want %v", what, got, err, c)
	}
}

// present asks a herdlet's hypervisor directly whether the actor's VM is there.
func present(t *testing.T, h *conformance.Herdlet, a *ateapi.Actor) vmm.Presence {
	t.Helper()
	p, err := h.VMM.Inspect(context.Background(), a.GetMetadata().GetUid())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return p
}

// load keeps the process busy until the returned stop is called: goroutines that spin on the CPU and goroutines that
// write and fsync files in dir, the way a loaded host (other test binaries, disk traffic) slows a herdlet's work.
func load(t *testing.T, dir string) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				if n%1024 == 0 {
					select {
					case <-done:
						return
					default:
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 4<<20)
			p := filepath.Join(dir, fmt.Sprintf("load-%d", i))
			for {
				select {
				case <-done:
					return
				default:
				}
				f, err := os.Create(p)
				if err != nil {
					return
				}
				_, _ = f.Write(buf)
				_ = f.Sync()
				_ = f.Close()
			}
		}()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

// coldBoot is the golden snapshot config of these tests' templates: they pin the cold-boot workflows (slots, owed
// steps, image resolutions), which a template's golden actor would take part in.
var coldBoot = &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}
