package conformance_test

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/prototext"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/vmm"
)

// privateMachines gives a herdlet Machines pulling through the real registry client with no node login
// (-registry-auth unset), as the herd herdlets run on 2026-11-05.
func privateMachines(t *testing.T) func(*herdlet.Agent) {
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
	}
}

// TestTemplatePullAuthBuildsTheGoldenSnapshot (PLT-330, H-M3): against an in-process private registry that refuses
// anonymous pulls (its token realm answers 403, as ghcr.io did for the delegate image) and herdlets with no node
// login, a template created with image_pull_auth builds its golden snapshot, and actors start from it; one created
// without it fails its golden build with the 403, naming the registry; one with a wrong credential fails naming the
// registry, not the credential. The secret is in no stored record, no GetActorTemplate answer and no log line.
// Before PLT-330 herd-api drops the field: the build with the credential fails 403 like the one without.
func TestTemplatePullAuthBuildsTheGoldenSnapshot(t *testing.T) {
	logs := captureLog(t)
	s := imagetest.NewServer()
	s.Push("everdict/everdict-sandbox", "hands", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "arm64"},
		Config: image.Config{Cmd: []string{"/bin/run"}},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "hands", Mode: 0o755})}})
	const user, password = "everdict-pull", "ghs_one-shot-pull-grant-3f9c"
	priv := &imagetest.Private{Registry: s, User: user, Password: password}
	ts := httptest.NewServer(priv)
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	img := host + "/everdict/everdict-sandbox:hands"
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	grant := `{"auths": {"` + host + `": {"auth": "` + auth + `"}}}`
	wrongPass := "ghs_wrong-grant-77aa"
	wrong := `{"auths": {"` + host + `": {"username": "` + user + `", "password": "` + wrongPass + `"}}}`

	e := conformance.Start(t, conformance.HerdletSpec{Name: "mac", Slots: 4, Caps: macCaps, Configure: privateMachines(t)})
	create := func(name, pullAuth string) {
		t.Helper()
		_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{
			ActorTemplate: &ateapi.ActorTemplate{
				Metadata:   &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
				Containers: []*ateapi.Container{{Name: "main", Image: img}},
			},
			ImagePullAuth: pullAuth,
		})
		if err != nil {
			t.Fatalf("CreateActorTemplate %s: %v", name, err)
		}
	}

	// Without a credential: today's behaviour, the anonymous pull's 403, naming the registry.
	create("anon", "")
	gs := waitGolden(t, e, "anon")
	if gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "403") || !strings.Contains(gs.GetErrorMessage(), host) {
		t.Errorf("golden build without image_pull_auth: %v, want an error naming the 403 and %s", gs, host)
	}

	// With it: the golden snapshot is built, and an actor starts from it.
	create("granted", grant)
	gs = waitGolden(t, e, "granted")
	if gs.GetGoldenTag() == nil {
		t.Fatalf("golden build with image_pull_auth: %v, want a golden tag", gs)
	}
	if priv.Requests().Tokens == 0 {
		t.Errorf("the registry issued no token: the pull did not log in")
	}
	a := mkActor(t, e, "a1", "granted", nil)
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("actor of the granted template: %v, want RUNNING", a.GetStatus().GetState())
	}

	// A credential the registry refuses: the build fails naming the registry, never the credential.
	create("wrong", wrong)
	gs = waitGolden(t, e, "wrong")
	msg := gs.GetErrorMessage()
	if gs.GetGoldenTag() != nil || !strings.Contains(msg, host) || !strings.Contains(msg, "401") {
		t.Errorf("golden build with a refused credential: %v, want an error naming %s and the 401", gs, host)
	}

	// A malformed credential is refused at create, quoting nothing of it.
	_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{
		ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "bad"},
			Containers: []*ateapi.Container{{Name: "main", Image: img}}},
		ImagePullAuth: `{"auths": {"` + host + `": {"auth": "` + password + `"}}}`,
	})
	wantCode(t, "CreateActorTemplate with a malformed image_pull_auth", err, codes.InvalidArgument)
	if err != nil && strings.Contains(err.Error(), password) {
		t.Errorf("the refusal quotes the credential: %v", err)
	}

	// One for no registry of the template's images, or for a template that builds no golden snapshot (the one pull
	// it is for), is refused rather than ignored.
	for what, req := range map[string]*ateapi.CreateActorTemplateRequest{
		"logins for another registry": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "elsewhere"},
			Containers: []*ateapi.Container{{Name: "main", Image: img}}},
			ImagePullAuth: `{"auths": {"ghcr.io": {"auth": "` + auth + `"}}}`},
		"golden DISABLED": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "cold"},
			Containers: []*ateapi.Container{{Name: "main", Image: img}}, SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot}},
			ImagePullAuth: grant},
	} {
		_, err := e.Client.CreateActorTemplate(ctxT(t), req)
		wantCode(t, "CreateActorTemplate with image_pull_auth, "+what, err, codes.InvalidArgument)
	}

	secrets := []string{password, auth, wrongPass}
	leaks := func(what, s string) {
		t.Helper()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Errorf("%s holds the credential %q", what, sec)
			}
		}
	}
	for _, name := range []string{"anon", "granted", "wrong"} {
		tpl, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, name)})
		if err != nil {
			t.Fatalf("GetActorTemplate %s: %v", name, err)
		}
		leaks("GetActorTemplate "+name, prototext.Format(tpl))
	}
	for _, k := range []store.Kind{store.KindActorTemplate, store.KindActor, store.KindTag, store.KindEgressPolicy, store.KindWorker, store.KindSpace} {
		for _, space := range []string{e.Space, ""} {
			recs, err := e.Store.List(ctxT(t), k, space)
			if err != nil {
				continue
			}
			for _, r := range recs {
				leaks("stored "+string(k)+" "+r.Name, string(r.Data))
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
	if !strings.Contains(out, "golden snapshot") {
		t.Fatalf("the captured log has no golden build line: the logger was not captured\n%s", out)
	}
	leaks("the log", out)
}
