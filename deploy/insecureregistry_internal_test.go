package deploy

import (
	"context"
	"encoding/base64"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// managedStore is the cluster's managed image store (PLT-42): plain HTTP on the tailnet.
const managedStore = "100.69.164.81:5001"

// TestInsecureRegistryIsPlainHTTP (criterion insecure-registry): -insecure-registry host[:port], as `herdlet run`,
// `herdlet join` and herd all-in-one parse it, makes the herdlet's image pulls speak plain HTTP to exactly the hosts
// it names, as the docker daemon's insecure-registries does; every other host stays https, the same address on
// another port included. The fake registry is plain HTTP and is dialed whatever address a pull names, so the pull
// reaches it as the managed store at 100.69.164.81:5001, a non-loopback host. It keeps the image behind a login, so
// -registry-auth's login for the host is what serves it. doctor lists the plain-HTTP hosts.
// Today no flag exists and only localhost and 127.0.0.1 are plain: the pull speaks TLS to the plain registry.
func TestInsecureRegistryIsPlainHTTP(t *testing.T) {
	s := imagetest.NewServer()
	amd := image.Platform{OS: "linux", Architecture: "amd64"}
	s.Push("digo-123c6584/digo-infra-delegate", "latest", imagetest.Manifest{Platform: amd,
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "f", Body: "x"})}})
	const user, password = "herd", "managed-store-login"
	priv := &imagetest.Private{Registry: s, User: user, Password: password}
	ts := httptest.NewServer(priv)
	defer ts.Close()
	// Every address a pull dials is the fake registry; the scheme it speaks is the client's choice for the host.
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ts.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	path := filepath.Join(t.TempDir(), "registry-auth.json")
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	if err := os.WriteFile(path, []byte(`{"auths": {"`+managedStore+`": {"auth": "`+auth+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	herdlets := map[string]func(args []string) (*Herdlet, error){
		"herdlet run": func(args []string) (*Herdlet, error) {
			h := &Herdlet{}
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			h.RunFlags(fs)
			return h, fs.Parse(args)
		},
		"herdlet join": func(args []string) (*Herdlet, error) {
			h := &Herdlet{}
			fs := flag.NewFlagSet("join", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			h.JoinFlags(fs)
			return h, fs.Parse(args)
		},
		"herd": func(args []string) (*Herdlet, error) {
			a := &AllInOne{}
			fs := flag.NewFlagSet("herd", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			a.Flags(fs)
			return &a.Herdlet, fs.Parse(args)
		},
	}
	pull := func(h *Herdlet, host string) error {
		ref, err := image.ParseRef(host + "/digo-123c6584/digo-infra-delegate:latest")
		if err != nil {
			t.Fatal(err)
		}
		c := h.imageSource()
		c.HTTP = &http.Client{Transport: transport}
		_, err = c.Resolve(context.Background(), ref, amd)
		return err
	}
	const tlsToPlain = "server gave HTTP response to HTTPS client"
	for name, parse := range herdlets {
		h, err := parse([]string{"-registry-auth", path})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := pull(h, managedStore); err == nil || !strings.Contains(err.Error(), tlsToPlain) {
			t.Errorf("%s without -insecure-registry: pull from %s: err = %v, want https refused by the plain registry", name, managedStore, err)
		}

		h, err = parse([]string{"-insecure-registry", managedStore, "-insecure-registry", "registry.lab:5000", "-registry-auth", path})
		if err != nil {
			t.Fatalf("%s -insecure-registry: %v", name, err)
		}
		before := priv.Requests().Tokens
		if err := pull(h, managedStore); err != nil {
			t.Errorf("%s -insecure-registry %s: pull: %v", name, managedStore, err)
		}
		if got := priv.Requests().Tokens; got == before {
			t.Errorf("%s: the pull from %s was not logged in with -registry-auth's login (tokens issued %d)", name, managedStore, got)
		}
		for _, other := range []string{"100.69.164.81:5002", "registry.example:5001", "100.69.164.82:5001"} {
			if err := pull(h, other); err == nil || !strings.Contains(err.Error(), tlsToPlain) {
				t.Errorf("%s: %s is not named, so it stays https: err = %v", name, other, err)
			}
		}
		d := doctorCheck(t, h, "insecure registries")
		if !d.OK || !strings.Contains(d.Detail, managedStore) || !strings.Contains(d.Detail, "registry.lab:5000") {
			t.Errorf("%s: doctor: %+v, want both plain-HTTP hosts listed", name, d)
		}

		h, err = parse([]string{"-insecure-registry", managedStore})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := pull(h, managedStore); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
			t.Errorf("%s -insecure-registry without -registry-auth: err = %v, want plain HTTP and the realm's anonymous 403", name, err)
		}

		for _, bad := range []string{"http://" + managedStore, managedStore + "/v2", "100.69.164.81:port"} {
			if _, err := parse([]string{"-insecure-registry", bad}); err == nil {
				t.Errorf("%s -insecure-registry %q: accepted, want host[:port] only", name, bad)
			}
		}
	}
	// The Nomad job's variable set to "" names no host.
	h, err := herdlets["herdlet join"]([]string{"-insecure-registry", ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := pull(h, managedStore); err == nil || !strings.Contains(err.Error(), tlsToPlain) {
		t.Errorf(`-insecure-registry "": err = %v, want https`, err)
	}
	d := doctorCheck(t, &Herdlet{}, "insecure registries")
	if !d.OK || !strings.Contains(d.Detail, "no -insecure-registry") {
		t.Errorf("doctor without -insecure-registry: %+v", d)
	}
}

func doctorCheck(t *testing.T, h *Herdlet, name string) Check {
	t.Helper()
	for _, c := range Doctor(h, Host{GOOS: "plan9", Stat: func(string) error { return nil }}) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("doctor has no %s line", name)
	return Check{}
}
