package deploy

import (
	"context"
	"encoding/base64"
	"flag"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// TestRegistryAuthReachesThePull (criterion private-pull): -registry-auth, as `herdlet run`, `herdlet join` and herd
// all-in-one parse it, is what the herdlet's image pulls log in with. A private image (a registry whose realm answers
// an anonymous token request 403, as ghcr.io did in the walk) is refused without the flag and with the file absent,
// and served once the file holds the login; doctor says which hosts the node has logins for and never the secret.
// Today there is no flag and every pull is anonymous: 403 Forbidden.
func TestRegistryAuthReachesThePull(t *testing.T) {
	s := imagetest.NewServer()
	s.Push("everdict/everdict-sandbox", "hands", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "f", Body: "x"})}})
	const user, password = "everdict", "ghp_private-pull"
	ts := httptest.NewServer(&imagetest.Private{Registry: s, User: user, Password: password})
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	ref, err := image.ParseRef(host + "/everdict/everdict-sandbox:hands")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry-auth.json")
	amd := image.Platform{OS: "linux", Architecture: "amd64"}

	herdlets := map[string]func(args []string) *Herdlet{
		"herdlet run": func(args []string) *Herdlet {
			h := &Herdlet{}
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			h.RunFlags(fs)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			return h
		},
		"herdlet join": func(args []string) *Herdlet {
			h := &Herdlet{}
			fs := flag.NewFlagSet("join", flag.ContinueOnError)
			h.JoinFlags(fs)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			return h
		},
		"herd": func(args []string) *Herdlet {
			a := &AllInOne{}
			fs := flag.NewFlagSet("herd", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			a.Flags(fs)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			return &a.Herdlet
		},
	}
	for name, parse := range herdlets {
		_ = os.Remove(path)
		pull := func(h *Herdlet) error {
			_, err := h.imageSource().Resolve(context.Background(), ref, amd)
			return err
		}
		if err := pull(parse(nil)); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
			t.Errorf("%s without -registry-auth: err = %v, want the realm's 403", name, err)
		}
		h := parse([]string{"-registry-auth", path})
		if h.RegistryAuth != path {
			t.Fatalf("%s: -registry-auth parsed as %q", name, h.RegistryAuth)
		}
		if err := pull(h); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
			t.Errorf("%s with the file absent: err = %v, want anonymous, the realm's 403", name, err)
		}
		if d := registryCheck(t, h); !d.OK || !strings.Contains(d.Detail, "anonymously") {
			t.Errorf("%s: doctor with the file absent: %+v", name, d)
		}
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
		if err := os.WriteFile(path, []byte(`{"auths": {"`+host+`": {"auth": "`+auth+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := pull(h); err != nil {
			t.Errorf("%s with the node's login: %v", name, err)
		}
		d := registryCheck(t, h)
		if !d.OK || !strings.Contains(d.Detail, host) || strings.Contains(d.Detail, password) || strings.Contains(d.Detail, auth) {
			t.Errorf("%s: doctor with the login: %+v", name, d)
		}
		if err := os.WriteFile(path, []byte(`{"auths": {"`+host+`": {"auth": "`+password+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if d := registryCheck(t, h); d.OK || strings.Contains(d.Detail, password) {
			t.Errorf("%s: doctor with a bad auth: %+v, want a failure that quotes no secret", name, d)
		}
	}
	if d := registryCheck(t, &Herdlet{}); !d.OK || !strings.Contains(d.Detail, "no -registry-auth") {
		t.Errorf("doctor without -registry-auth: %+v", d)
	}
}

func registryCheck(t *testing.T, h *Herdlet) Check {
	t.Helper()
	for _, c := range Doctor(h, Host{GOOS: "plan9", Stat: func(string) error { return nil }}) {
		if c.Name == "registry auth" {
			return c
		}
	}
	t.Fatal("doctor has no registry auth line")
	return Check{}
}
