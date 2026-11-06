package conformance_test

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
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

// storeMachines gives a herdlet Machines pulling through the real registry client with no node login and no
// -insecure-registry, whose every connection reaches the plain-HTTP server at addr whatever host it names: the ref
// can name a store on any cluster's address, and the scheme the client picks shows in the pull's error (https to the
// plain server fails "server gave HTTP response to HTTPS client").
func storeMachines(t *testing.T, addr string) func(*herdlet.Agent) {
	dial := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	return func(a *herdlet.Agent) {
		caps, err := a.VMM.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		m, err := machine.New(machine.Config{VMM: a.VMM, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
			Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: &registry.Client{HTTP: &http.Client{Transport: dial}},
				Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Machines = m
	}
}

// TestTemplatePlainHostsBuildTheGoldenSnapshot (DEFAUL-1600, criterion a-pull-names-its-plain-hosts): an image store
// run without TLS, on an address that is no loopback, with its token realm on another host, and herdlets with no
// -insecure-registry and no node login. A template whose create names the store and its realm in
// image_plain_http_hosts, beside its image_pull_auth, builds its golden snapshot; without the field the pull goes
// https and fails; naming the store but not its realm, the login is not sent to the plain realm. The hosts are the
// golden pull's only: a later template's pull from the same store without them goes https again. They are checked at
// create (host[:port] only, a golden build to be for), and kept in no stored record and no GetActorTemplate answer.
func TestTemplatePlainHostsBuildTheGoldenSnapshot(t *testing.T) {
	const storeHost, realmHost = "store.cluster.test:5001", "realm.cluster.test:8787"
	s := imagetest.NewServer()
	for _, tag := range []string{"hands", "later"} {
		s.Push("everdict/everdict-sandbox", tag, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "arm64"},
			Config: image.Config{Cmd: []string{"/bin/run"}},
			Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: tag, Mode: 0o755})}})
	}
	const user, password = "everdict-pull", "ghs_one-shot-pull-grant-4b1d"
	priv := &imagetest.Private{Registry: s, User: user, Password: password, Realm: realmHost}
	ts := httptest.NewServer(priv)
	t.Cleanup(ts.Close)
	img := storeHost + "/everdict/everdict-sandbox:"
	grant := `{"auths": {"` + storeHost + `": {"auth": "` + base64.StdEncoding.EncodeToString([]byte(user+":"+password)) + `"}}}`

	e := conformance.Start(t, conformance.HerdletSpec{Name: "mac", Slots: 4, Caps: macCaps, Configure: storeMachines(t, ts.Listener.Addr().String())})
	create := func(name, tag string, plain []string) {
		t.Helper()
		_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{
			ActorTemplate: &ateapi.ActorTemplate{
				Metadata:   &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
				Containers: []*ateapi.Container{{Name: "main", Image: img + tag}},
			},
			ImagePullAuth:       grant,
			ImagePlainHttpHosts: plain,
		})
		if err != nil {
			t.Fatalf("CreateActorTemplate %s: %v", name, err)
		}
	}

	// Without the field: https to the plain store, as before.
	create("unnamed", "hands", nil)
	gs := waitGolden(t, e, "unnamed")
	if gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "server gave HTTP response to HTTPS client") {
		t.Errorf("golden build without image_plain_http_hosts: %v, want the https attempt's error", gs)
	}

	// The store named, its realm not: the realm is not https, and the login is not sent to it.
	create("store-only", "hands", []string{storeHost})
	gs = waitGolden(t, e, "store-only")
	if gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "realm http://"+realmHost+"/token is not https") {
		t.Errorf("golden build naming the store but not its realm: %v, want the realm refused for the login", gs)
	}
	if r := priv.Requests(); r.Tokens != 0 || r.BadLogins != 0 || r.AnonymousTokens != 0 {
		t.Errorf("the realm was asked for a token (%+v) before any build named it plain", r)
	}

	// Both named: the golden snapshot is built over plain HTTP with the login, and an actor starts from it.
	create("named", "hands", []string{storeHost, strings.ToUpper(realmHost)})
	gs = waitGolden(t, e, "named")
	if gs.GetGoldenTag() == nil {
		t.Fatalf("golden build naming the store and its realm: %v, want a golden tag", gs)
	}
	if priv.Requests().Tokens == 0 {
		t.Errorf("the realm issued no token: the pull did not log in")
	}
	a := mkActor(t, e, "a1", "named", nil)
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("actor of the template: %v, want RUNNING", a.GetStatus().GetState())
	}

	// The hosts were that pull's only: the same herdlet's next pull from the store, without them, is https.
	create("later", "later", nil)
	gs = waitGolden(t, e, "later")
	if gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "server gave HTTP response to HTTPS client") {
		t.Errorf("a later golden build without image_plain_http_hosts: %v, want https again", gs)
	}

	// Checked at create: a host[:port] each, for a template that builds a golden snapshot.
	for what, req := range map[string]*ateapi.CreateActorTemplateRequest{
		"a URL": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "url"},
			Containers: []*ateapi.Container{{Name: "main", Image: img + "hands"}}}, ImagePlainHttpHosts: []string{"http://" + storeHost}},
		"a path": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "path"},
			Containers: []*ateapi.Container{{Name: "main", Image: img + "hands"}}}, ImagePlainHttpHosts: []string{storeHost + "/token"}},
		"empty": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "empty"},
			Containers: []*ateapi.Container{{Name: "main", Image: img + "hands"}}}, ImagePlainHttpHosts: []string{""}},
		"golden DISABLED": {ActorTemplate: &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "cold"},
			Containers: []*ateapi.Container{{Name: "main", Image: img + "hands"}}, SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot}},
			ImagePlainHttpHosts: []string{storeHost}},
	} {
		_, err := e.Client.CreateActorTemplate(ctxT(t), req)
		wantCode(t, "CreateActorTemplate with image_plain_http_hosts, "+what, err, codes.InvalidArgument)
	}

	// Never kept: the realm's host is named nowhere else, so no stored record or answer holds it.
	for _, name := range []string{"unnamed", "store-only", "named", "later"} {
		tpl, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, name)})
		if err != nil {
			t.Fatalf("GetActorTemplate %s: %v", name, err)
		}
		if out := prototext.Format(tpl); strings.Contains(strings.ToLower(out), "realm.cluster.test") {
			t.Errorf("GetActorTemplate %s holds the plain hosts: %s", name, out)
		}
	}
	for _, k := range []store.Kind{store.KindActorTemplate, store.KindActor, store.KindTag} {
		recs, err := e.Store.List(ctxT(t), k, e.Space)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if strings.Contains(strings.ToLower(string(r.Data)), "realm.cluster.test") {
				t.Errorf("stored %s %s holds the plain hosts", k, r.Name)
			}
		}
	}
}
