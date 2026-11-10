package conformance_test

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/prototext"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
)

// TestActorPullAuthBootsCold (PLT-405): a template's golden snapshot is a MEMORY snapshot of the restore family it
// was built on (tiger). An actor placed on a node of another family (lunar) cannot restore it and boots cold, which
// pulls the private image; lunar has no node login, as nahyunii-940XHA had none for the managed store on 2026-11-10.
// A create with no grant fails as it does today, with the anonymous pull's 403 naming the registry. One carrying
// image_pull_auth boots, and that boot alone carries the grant (with image_plain_http_hosts): no later work on the
// actor, including a boot that settles an owed create, has it, and it is in no stored record, no answer and no log
// line. Before PLT-405 herd-api ignores the fields and the granted create fails like the anonymous one.
func TestActorPullAuthBootsCold(t *testing.T) {
	logs := captureLog(t)
	s := imagetest.NewServer()
	s.Push("everdict/everdict-sandbox", "hands", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Config: image.Config{Cmd: []string{"/bin/run"}},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "hands", Mode: 0o755})}})
	const user, password = "everdict-pull", "ghs_one-shot-actor-grant-5b2e"
	priv := &imagetest.Private{Registry: s, User: user, Password: password}
	ts := httptest.NewServer(priv)
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	img := host + "/everdict/everdict-sandbox:hands"
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	grant := `{"auths": {"` + host + `": {"auth": "` + auth + `"}}}`

	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "tiger", Slots: 4, Labels: map[string]string{"host": "tiger"}, Caps: chCaps(famA), Configure: privateMachines(t)},
		conformance.HerdletSpec{Name: "lunar", Slots: 4, Labels: map[string]string{"host": "lunar"}, Caps: chCaps(famB), Configure: privateMachines(t)})
	var lunar workLog
	lunar.watch(e.Herdlets["lunar"])

	// The template's golden build pulls with the template's own grant, on tiger.
	if _, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{
		ActorTemplate: &ateapi.ActorTemplate{
			Metadata:       &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"},
			Containers:     []*ateapi.Container{{Name: "main", Image: img}},
			WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"host": "tiger"}},
		},
		ImagePullAuth: grant,
	}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	gs := waitGolden(t, e, "tpl")
	if gs.GetGoldenTag() == nil {
		t.Fatalf("golden build: %v, want a golden tag", gs)
	}
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatalf("GetTag golden: %v", err)
	}
	if snap := tag.GetStatus().GetSnapshot(); snap.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || snap.GetLocality() != famA {
		t.Fatalf("golden snapshot %v of %q, want MEMORY of %q", snap.GetFidelity(), snap.GetLocality(), famA)
	}

	onLunar := func(name, pullAuth string, plain []string) *ateapi.CreateActorRequest {
		r := actorReq(e, name, "tpl", map[string]string{"host": "lunar"})
		r.ImagePullAuth, r.ImagePlainHttpHosts = pullAuth, plain
		return r
	}

	// No grant: as today, the cold boot's anonymous pull is refused, naming the registry, and no actor is left.
	_, err = e.Client.CreateActor(ctxT(t), onLunar("anon", "", nil))
	wantCode(t, "CreateActor on lunar without image_pull_auth", err, codes.FailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), host) {
		t.Errorf("CreateActor on lunar without image_pull_auth: %v, want the 403 naming %s", err, host)
	}
	if _, err := e.Client.GetActor(ctxT(t), &ateapi.GetActorRequest{Actor: oref(e, "anon")}); err == nil {
		t.Errorf("a create whose boot failed left the actor")
	}

	// The grant: the cold boot on lunar logs in with it and runs.
	tokens := priv.Requests().Tokens
	a, err := e.Client.CreateActor(ctxT(t), onLunar("a1", grant, []string{host}))
	if err != nil {
		t.Fatalf("CreateActor on lunar with image_pull_auth: %v", err)
	}
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || a.GetStatus().GetAssignedNode() != "lunar" {
		t.Fatalf("CreateActor a1: %v on %q, want RUNNING on lunar", a.GetStatus().GetState(), a.GetStatus().GetAssignedNode())
	}
	if priv.Requests().Tokens == tokens {
		t.Errorf("the registry issued no token for a1: its pull did not log in")
	}
	uid := a.GetMetadata().GetUid()
	work := lunar.of(uid)
	if len(work) == 0 || work[0].Op != link.OpBoot || work[0].PullAuth != link.Secret(grant) || len(work[0].PlainHosts) != 1 || work[0].PlainHosts[0] != host {
		t.Fatalf("a1's herdlet work %+v, want a cold boot carrying the grant and plain host %s", work, host)
	}

	// Later work on a1 never carries it.
	if _, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}}); err != nil {
		t.Fatalf("SuspendActor a1: %v", err)
	}
	if _, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("ResumeActor a1: %v", err)
	}
	for _, w := range lunar.of(uid)[1:] {
		if w.PullAuth != "" || len(w.PlainHosts) > 0 {
			t.Errorf("later work on a1 carries the grant: %s %+v", w.Op, w)
		}
	}

	// A create whose boot answered unknown is owed; the boot that settles it is a later boot, and has no grant.
	var hidden atomic.Bool
	e.Herdlets["lunar"].Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		lunar.mu.Lock()
		lunar.work = append(lunar.work, w)
		lunar.mu.Unlock()
		if w.Op == link.OpBoot && w.Actor != nil && w.Actor.Name == "a2" && hidden.CompareAndSwap(false, true) {
			return link.Outcome{Result: link.ResultUnknown, Reason: "answer lost"}, true
		}
		return o, true
	})
	_, err = e.Client.CreateActor(ctxT(t), onLunar("a2", grant, nil))
	wantCode(t, "CreateActor a2 whose boot answered unknown", err, codes.Unavailable)
	a2, _ := getActor(t, e, "a2")
	if _, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("ResumeActor a2 (settles the owed boot): %v", err)
	}
	work = lunar.of(a2.GetMetadata().GetUid())
	if len(work) < 2 || work[0].PullAuth != link.Secret(grant) {
		t.Fatalf("a2's herdlet work %+v, want its first boot with the grant and the settling work after it", work)
	}
	boots := 0
	for _, w := range work[1:] {
		if w.Op == link.OpBoot {
			boots++
		}
		if w.PullAuth != "" || len(w.PlainHosts) > 0 {
			t.Errorf("work settling a2's owed create carries the grant: %s %+v", w.Op, w)
		}
	}
	if boots == 0 {
		t.Errorf("a2's owed create was settled with no boot: %+v", work)
	}

	// Refused as the template's fields are: malformed (quoting nothing of it), for another registry, a bad plain host.
	for what, req := range map[string]*ateapi.CreateActorRequest{
		"malformed image_pull_auth":         onLunar("bad1", `{"auths": {"`+host+`": {"auth": "`+password+`"}}}`, nil),
		"logins for another registry":       onLunar("bad2", `{"auths": {"ghcr.io": {"auth": "`+auth+`"}}}`, nil),
		"an image_plain_http_hosts with /x": onLunar("bad3", grant, []string{host + "/x"}),
	} {
		_, err := e.Client.CreateActor(ctxT(t), req)
		wantCode(t, "CreateActor with "+what, err, codes.InvalidArgument)
		if err != nil && strings.Contains(err.Error(), password) {
			t.Errorf("the refusal of %s quotes the credential: %v", what, err)
		}
	}

	secrets := []string{password, auth}
	leaks := func(what, s string) {
		t.Helper()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Errorf("%s holds the credential %q", what, sec)
			}
		}
	}
	for _, name := range []string{"a1", "a2"} {
		a, _ := getActor(t, e, name)
		leaks("GetActor "+name, prototext.Format(a))
	}
	for _, k := range []store.Kind{store.KindActorTemplate, store.KindActor, store.KindTag, store.KindEgressPolicy, store.KindWorker, store.KindSpace} {
		for _, space := range []string{e.Space, ""} {
			recs, err := e.Store.List(ctxT(t), k, space)
			if err != nil {
				continue
			}
			for _, r := range recs {
				leaks("stored "+string(k)+" "+r.Name, string(r.Data)+string(r.State))
			}
		}
	}
	if err := filepath.Walk(t.TempDir()+"/..", func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() > 1<<20 {
			return nil
		}
		b, _ := os.ReadFile(p)
		leaks("file "+p, string(b))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "actor "+e.Space+"/a1: create") {
		t.Fatalf("the captured log has no line of a1's create: the logger was not captured\n%s", out)
	}
	leaks("the log", out)
}
