package artifacts_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/deploy/artifacts"
)

const dist = "../../dist"

// TestDistIsRendered: dist/ holds exactly what the renderer produces (go generate ./deploy/artifacts re-renders it).
func TestDistIsRendered(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, f := range files {
		want[f.Path] = true
		got, err := os.ReadFile(filepath.Join(dist, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Errorf("%s: %v (run go generate ./deploy/artifacts)", f.Path, err)
			continue
		}
		if !bytes.Equal(got, f.Data) {
			t.Errorf("dist/%s differs from its template's rendering (run go generate ./deploy/artifacts)", f.Path)
		}
	}
	err = filepath.WalkDir(dist, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dist, p)
		if rel = filepath.ToSlash(rel); !want[rel] {
			t.Errorf("dist/%s is not rendered by any template", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestArtifactsRunCommandLinesTheBinariesAccept: every command line a unit, plist or Nomad task runs parses with the
// binary's own flag set (deploy.Parse, what the binary's main runs), so no artifact names a flag, subcommand or
// argument the binary would refuse.
func TestArtifactsRunCommandLinesTheBinariesAccept(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if len(invs) == 0 {
			t.Errorf("%s runs no command", f.Path)
		}
		for _, inv := range invs {
			r, err := deploy.Parse(inv.Binary, inv.Args, io.Discard)
			if err != nil {
				t.Errorf("%s: %s %s: %v", f.Path, inv.Binary, strings.Join(inv.Args, " "), err)
				continue
			}
			seen[string(f.Kind)+":"+r.Command.Name()] = true
		}
	}
	for _, k := range []string{
		"systemd:herd", "systemd:herd-api", "systemd:herd-router", "systemd:herdlet join", "systemd:herd-egress",
		"launchd:herd", "launchd:herdlet join",
		"nomad:herd-api", "nomad:herd-router", "nomad:herdlet join", "nomad:herd-egress",
	} {
		if !seen[k] {
			t.Errorf("no %s artifact runs %s", strings.SplitN(k, ":", 2)[0], strings.SplitN(k, ":", 2)[1])
		}
	}
}

// TestNomadJobPair: herd-api is a service job and herdlet a system job; the herdlet job joins with the token in its
// Nomad variable.
func TestNomadJobPair(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"nomad/herd-api.nomad.hcl", `type = "service"`},
		{"nomad/herdlet.nomad.hcl", `type = "system"`},
		{"nomad/herdlet.nomad.hcl", `nomadVar \"nomad/jobs/herdlet\"`},
		{"nomad/herdlet.nomad.hcl", `nomadService \"herd-api\"`},
	} {
		b, err := os.ReadFile(filepath.Join(dist, c.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%s lacks %s", c.file, c.want)
		}
	}
}
