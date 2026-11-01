package conformance_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/vmm"
)

// pinRegistry is a registry over HTTP, spoken to by the real registry client, holding the two images of the
// two-container template for both architectures under tag 1; body says what each image's bin/run holds.
type pinRegistry struct {
	*imagetest.Server
	host string
}

func newPinRegistry(t *testing.T, body string) *pinRegistry {
	s := imagetest.NewServer()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	r := &pinRegistry{Server: s, host: strings.TrimPrefix(ts.URL, "http://")}
	r.push(body)
	return r
}

// push pushes both images under tag 1 again with new contents: the tag moves.
func (r *pinRegistry) push(body string) {
	for _, repo := range []string{"everdict/agent", "everdict/sidecar"} {
		var ms []imagetest.Manifest
		for _, arch := range []string{"amd64", "arm64"} {
			ms = append(ms, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: arch},
				Config: image.Config{Cmd: []string{"/bin/run"}},
				Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: body + "-" + arch, Mode: 0o755})}})
		}
		r.Push(repo, "1", ms...)
	}
}

// pinMachines gives a herdlet Machines whose images come from reg through the real registry client.
func pinMachines(t *testing.T, reg *pinRegistry, out **machine.Machines) func(*herdlet.Agent) {
	return func(a *herdlet.Agent) {
		caps, err := a.VMM.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		m, err := machine.New(machine.Config{VMM: a.VMM, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
			Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: &registry.Client{}, Builder: &imagetest.Builder{},
				Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Machines = m
		*out = m
	}
}

func pinTemplate(t *testing.T, e *conformance.Env, reg *pinRegistry, name string) {
	t.Helper()
	pinTemplateAt(t, e, reg, name, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
}

func pinTemplateAt(t *testing.T, e *conformance.Env, reg *pinRegistry, name string, f ateapi.SnapshotFidelity) {
	t.Helper()
	_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
		Containers: []*ateapi.Container{
			{Name: "agent", Image: reg.host + "/everdict/agent:1", VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}},
			{Name: "sidecar", Image: reg.host + "/everdict/sidecar:1", VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}},
		},
		Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: f, GoldenSnapshotConfig: coldBoot},
	}})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
}

// runs is what bin/run holds in every image the actor's VM on m runs (the test builder's listing names its size,
// so the bodies of the test differ in length).
func runs(t *testing.T, m *machine.Machines, uid string) []string {
	t.Helper()
	imgs := m.Images(uid)
	if len(imgs) != 2 {
		t.Fatalf("actor %s runs %d images, want 2", uid, len(imgs))
	}
	var out []string
	for _, r := range imgs {
		b, err := os.ReadFile(r.Path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func listing(body string) string {
	return "d0755 0:0 0 bin/\n-0755 0:0 " + strconv.Itoa(len(body)) + " bin/run\n"
}

func sameAll(t *testing.T, what string, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g != want {
			t.Errorf("%s: an image holds %q, want %q", what, g, want)
		}
	}
}

// TestTagResolvedOnceAndPinned is the pin-and-cache walk on two fake hosts of two architectures (a Mac, arm64, and
// an Ubuntu host, amd64) against a registry that counts what it is asked: N creates of one template resolve each tag
// once; a suspend, a resume on the same host, a move to the other host and a fork resolve no tag, and the resume on
// the host that has the images reaches no registry at all. Today (before PLT-213) every boot and restore resolves
// every tag, and so does every snapshot.
func TestTagResolvedOnceAndPinned(t *testing.T) {
	reg := newPinRegistry(t, "v1")
	var mac, ubuntu *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 4, Labels: map[string]string{"host": "mac"}, Caps: macCaps, Configure: pinMachines(t, reg, &mac)},
		conformance.HerdletSpec{Name: "ubuntu", Slots: 4, Labels: map[string]string{"host": "ubuntu"}, Caps: ubuntuCaps, Configure: pinMachines(t, reg, &ubuntu)})
	pinTemplate(t, e, reg, "tpl")
	for _, n := range []string{"a1", "a2", "a3"} {
		mkActor(t, e, n, "tpl", map[string]string{"host": "mac"})
	}
	if got := reg.Requests().TagManifests; got != 2 {
		t.Fatalf("3 creates of one two-image template resolved a tag %d times, want 2 (once per reference)", got)
	}
	suspend(t, e, "a1")
	if got := reg.Requests().TagManifests; got != 2 {
		t.Fatalf("a suspend resolved a tag: %d tag resolves, want 2", got)
	}
	before := reg.Requests()
	r, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	if err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	if got := reg.Requests(); got != before {
		t.Fatalf("a resume on the host that has the images asked the registry: %+v, before %+v", got, before)
	}
	sameAll(t, "resumed a1", runs(t, mac, r.GetActor().GetMetadata().GetUid()), listing("v1-arm64"))

	suspend(t, e, "a2")
	a2 := moveTo(t, e, "a2", "ubuntu")
	got := reg.Requests()
	if got.TagManifests != 2 {
		t.Fatalf("a move to another host resolved a tag: %d tag resolves, want 2", got.TagManifests)
	}
	if got.DigestManifests == before.DigestManifests {
		t.Fatalf("the move's host fetched no manifest by digest: %+v", got)
	}
	sameAll(t, "moved a2", runs(t, ubuntu, a2.GetMetadata().GetUid()), listing("v1-amd64"))

	if _, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "a3"),
	}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	for _, host := range []string{"mac", "ubuntu"} {
		req := actorReq(e, "fork-"+host, "tpl", map[string]string{"host": host})
		req.Actor.SourceTag = oref(e, "t1")
		if _, err := e.Client.CreateActor(ctxT(t), req); err != nil {
			t.Fatalf("fork onto %s: %v", host, err)
		}
	}
	if got := reg.Requests().TagManifests; got != 2 {
		t.Fatalf("forks resolved a tag: %d tag resolves, want 2", got)
	}
}

// TestMovedTagDoesNotChangeAResumedActor: an actor created when tag 1 named v1 runs v1 after the tag moved to v2,
// resumed on its host, moved to a host of another architecture and forked; only a new template sees v2. Today the
// restore resolves the tag again and runs v2 under the volumes v1 left.
func TestMovedTagDoesNotChangeAResumedActor(t *testing.T) {
	reg := newPinRegistry(t, "v1")
	var mac, ubuntu *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 4, Labels: map[string]string{"host": "mac"}, Caps: macCaps, Configure: pinMachines(t, reg, &mac)},
		conformance.HerdletSpec{Name: "ubuntu", Slots: 4, Labels: map[string]string{"host": "ubuntu"}, Caps: ubuntuCaps, Configure: pinMachines(t, reg, &ubuntu)})
	pinTemplate(t, e, reg, "tpl")
	a := mkActor(t, e, "a1", "tpl", map[string]string{"host": "mac"})
	uid := a.GetMetadata().GetUid()
	sameAll(t, "created a1", runs(t, mac, uid), listing("v1-arm64"))
	suspend(t, e, "a1")
	reg.push("v2-moved")

	if _, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	sameAll(t, "a1 resumed after the tag moved", runs(t, mac, uid), listing("v1-arm64"))
	suspend(t, e, "a1")
	moveTo(t, e, "a1", "ubuntu")
	sameAll(t, "a1 moved after the tag moved", runs(t, ubuntu, uid), listing("v1-amd64"))

	if _, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "a1"),
	}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	req := actorReq(e, "fork", "tpl", map[string]string{"host": "mac"})
	req.Actor.SourceTag = oref(e, "t1")
	fork, err := e.Client.CreateActor(ctxT(t), req)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	sameAll(t, "fork after the tag moved", runs(t, mac, fork.GetMetadata().GetUid()), listing("v1-arm64"))

	// A template created after the move resolves the tag it names, and runs what it names now.
	pinTemplate(t, e, reg, "tpl-new")
	b := mkActor(t, e, "b1", "tpl-new", map[string]string{"host": "mac"})
	sameAll(t, "an actor of a new template", runs(t, mac, b.GetMetadata().GetUid()), listing("v2-moved-arm64"))
}

// TestRegistryRefusalIsUnavailable: a registry that answers 429 and then serves is retried, and the create succeeds;
// one that keeps refusing past the herdlet's bounded backoff, or asks for a wait past the work's deadline, fails the
// create as Unavailable, not as a failure of the actor (FailedPrecondition). Today the first 429 fails the boot.
// (That the wait is the Retry-After's is image/registry's TestRetryAfterHonoured: the harness's herdlet deadline is
// 400 ms.)
func TestRegistryRefusalIsUnavailable(t *testing.T) {
	reg := newPinRegistry(t, "v1")
	var mac *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 4, Labels: map[string]string{"host": "mac"}, Caps: macCaps, Configure: pinMachines(t, reg, &mac)})
	pinTemplate(t, e, reg, "tpl")
	reg.Refuse(imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: "0"})
	mkActor(t, e, "a1", "tpl", map[string]string{"host": "mac"})
	if got := reg.Requests().Refused; got != 1 {
		t.Fatalf("%d requests refused, want 1", got)
	}

	pinTemplate(t, e, reg, "tpl-2")
	refusals := make([]imagetest.Answer, registry.DefaultRetries+1)
	for i := range refusals {
		refusals[i] = imagetest.Answer{Status: http.StatusTooManyRequests, RetryAfter: "0"}
	}
	reg.Refuse(refusals...)
	_, err := e.Client.CreateActor(ctxT(t), actorReq(e, "a2", "tpl-2", map[string]string{"host": "mac"}))
	wantCode(t, "CreateActor against a registry that keeps answering 429", err, codes.Unavailable)
	if got := reg.Requests().Refused; got != 1+registry.DefaultRetries+1 {
		t.Errorf("%d requests refused, want %d (the first refusal, then one try and %d retries)", got, 1+registry.DefaultRetries+1, registry.DefaultRetries)
	}

	// A Retry-After the herdlet's work deadline would cut is not waited out into a herdlet that does not answer
	// (a create owed): it is answered as Unavailable at once.
	pinTemplate(t, e, reg, "tpl-3")
	refused := reg.Requests().Refused
	reg.Refuse(imagetest.Answer{Status: http.StatusServiceUnavailable, RetryAfter: "5"})
	_, err = e.Client.CreateActor(ctxT(t), actorReq(e, "a3", "tpl-3", map[string]string{"host": "mac"}))
	wantCode(t, "CreateActor against a registry that answers 503 with a Retry-After past the deadline", err, codes.Unavailable)
	if err != nil && strings.Contains(err.Error(), "owed") {
		t.Errorf("the create is owed, not answered: %v", err)
	}
	if got := reg.Requests().Refused - refused; got != 1 {
		t.Errorf("%d requests refused, want 1 (no retry past the deadline)", got)
	}
}

// TestMemoryMoveAndForkRunThePinnedImage: on two Cloud Hypervisor hosts of one family, an actor suspended at MEMORY
// (the bundle carries its memory, PLT-212) and resumed on the other host after its tag moved runs the image it was
// created with, as do forks of it on both hosts, and none of them resolves a tag.
func TestMemoryMoveAndForkRunThePinnedImage(t *testing.T) {
	reg := newPinRegistry(t, "v1")
	var u1, u2 *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "u1", Slots: 4, Labels: map[string]string{"host": "u1"}, Caps: chCaps(famA), Configure: pinMachines(t, reg, &u1)},
		conformance.HerdletSpec{Name: "u2", Slots: 4, Labels: map[string]string{"host": "u2"}, Caps: chCaps(famA), Configure: pinMachines(t, reg, &u2)})
	pinTemplateAt(t, e, reg, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", map[string]string{"host": "u1"})
	uid := a.GetMetadata().GetUid()
	if s := suspend(t, e, "a1"); s.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Fatalf("suspended at %v, want MEMORY", s.GetFidelity())
	}
	reg.push("v2-moved")
	tags := reg.Requests().TagManifests
	moveTo(t, e, "a1", "u2")
	sameAll(t, "a1 moved at MEMORY after the tag moved", runs(t, u2, uid), listing("v1-amd64"))
	if _, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "a1"),
	}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	for _, host := range []string{"u1", "u2"} {
		req := actorReq(e, "fork-"+host, "tpl", map[string]string{"host": host})
		req.Actor.SourceTag = oref(e, "t1")
		fork, err := e.Client.CreateActor(ctxT(t), req)
		if err != nil {
			t.Fatalf("fork onto %s: %v", host, err)
		}
		m := map[string]*machine.Machines{"u1": u1, "u2": u2}[host]
		sameAll(t, "fork on "+host, runs(t, m, fork.GetMetadata().GetUid()), listing("v1-amd64"))
	}
	if got := reg.Requests().TagManifests; got != tags {
		t.Fatalf("a MEMORY move and two forks resolved a tag %d times, want 0", got-tags)
	}
}
