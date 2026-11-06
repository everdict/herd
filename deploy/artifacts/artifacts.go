// Package artifacts renders herd's install artifacts: systemd units (Linux), launchd property lists (macOS) and the
// Nomad job pair. The rendered files live in dist/ at the repository root and are what the runbooks install;
// `go generate ./deploy/artifacts` re-renders them, and a test keeps dist/ equal to the renderer's output and every
// flag an artifact passes accepted by the binary it runs.
package artifacts

//go:generate go run ../../internal/cmd/renderdist -out ../../dist

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templates embed.FS

// Params are the paths the artifacts name.
type Params struct {
	BinDir          string
	DataDir         string
	SnapshotDir     string
	HerdletStateDir string
	GuestDir        string
	// LibexecDir holds the virtiofsd the Linux release archive carries (deploy.DefaultVirtiofsd).
	LibexecDir string
	// InsecureRegistries is the Nomad herdlet job's default -insecure-registry, a node-level fallback: none. A
	// template's golden pull names its own plain-HTTP hosts (image_plain_http_hosts, DEFAUL-1600), so no job names a
	// cluster's store.
	InsecureRegistries string

	MacBinDir          string
	MacDataDir         string
	MacEtcDir          string
	MacHerdletStateDir string
	MacGuestDir        string
}

// Defaults are the paths the shipped artifacts use.
var Defaults = Params{
	BinDir: "/usr/local/bin", DataDir: "/var/lib/herd", SnapshotDir: "/var/lib/herd/snapshots",
	HerdletStateDir: "/var/lib/herdlet", GuestDir: "/usr/local/share/herd", LibexecDir: "/usr/local/libexec/herd",
	InsecureRegistries: "",
	MacBinDir:          "/usr/local/bin", MacDataDir: "/usr/local/var/herd", MacEtcDir: "/usr/local/etc/herd",
	MacHerdletStateDir: "/usr/local/var/herdlet", MacGuestDir: "/usr/local/share/herd",
}

// Kind is an artifact's format.
type Kind string

const (
	Systemd Kind = "systemd"
	Launchd Kind = "launchd"
	Nomad   Kind = "nomad"
)

// File is one rendered artifact; Path is relative to dist/.
type File struct {
	Path string
	Kind Kind
	Data []byte
}

func kindOf(name string) (Kind, string) {
	switch {
	case strings.HasSuffix(name, ".service"):
		return Systemd, "systemd/" + name
	case strings.HasSuffix(name, ".plist"):
		return Launchd, "launchd/" + name
	case strings.HasSuffix(name, ".nomad.hcl"):
		return Nomad, "nomad/" + name
	}
	return "", ""
}

// Render renders every artifact with p, ordered by path.
func Render(p Params) ([]File, error) {
	names, err := templates.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	var out []File
	for _, n := range names {
		name := strings.TrimSuffix(n.Name(), ".tmpl")
		kind, rel := kindOf(name)
		if kind == "" {
			return nil, fmt.Errorf("artifacts: template %s has no known kind", n.Name())
		}
		src, err := templates.ReadFile(path.Join("templates", n.Name()))
		if err != nil {
			return nil, err
		}
		t, err := template.New(name).Delims("[[", "]]").Option("missingkey=error").Parse(string(src))
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, p); err != nil {
			return nil, err
		}
		out = append(out, File{Path: rel, Kind: kind, Data: b.Bytes()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Invocation is one command line an artifact runs: the binary's base name and its arguments.
type Invocation struct {
	Binary string
	Args   []string
}

// Invocations extracts the command lines f runs: ExecStart= of a unit, ProgramArguments of a plist, and each task's
// command and args in a Nomad job.
func Invocations(f File) ([]Invocation, error) {
	switch f.Kind {
	case Systemd:
		return systemdInvocations(string(f.Data))
	case Launchd:
		return launchdInvocations(string(f.Data))
	case Nomad:
		return nomadInvocations(string(f.Data))
	}
	return nil, fmt.Errorf("artifacts: %s: unknown kind", f.Path)
}

func invocation(argv []string) (Invocation, error) {
	if len(argv) == 0 {
		return Invocation{}, fmt.Errorf("artifacts: empty command line")
	}
	return Invocation{Binary: path.Base(argv[0]), Args: argv[1:]}, nil
}

func systemdInvocations(s string) ([]Invocation, error) {
	var out []Invocation
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		l, ok := strings.CutPrefix(lines[i], "ExecStart=")
		if !ok {
			continue
		}
		for strings.HasSuffix(l, "\\") && i+1 < len(lines) {
			i++
			l = strings.TrimSuffix(l, "\\") + " " + strings.TrimSpace(lines[i])
		}
		inv, err := invocation(strings.Fields(l))
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

func launchdInvocations(s string) ([]Invocation, error) {
	_, rest, ok := strings.Cut(s, "<key>ProgramArguments</key>")
	if !ok {
		return nil, fmt.Errorf("artifacts: plist has no ProgramArguments")
	}
	body, _, ok := strings.Cut(rest, "</array>")
	if !ok {
		return nil, fmt.Errorf("artifacts: ProgramArguments is not an array")
	}
	var argv []string
	for _, part := range strings.Split(body, "<string>")[1:] {
		v, _, ok := strings.Cut(part, "</string>")
		if !ok {
			return nil, fmt.Errorf("artifacts: unterminated <string> in ProgramArguments")
		}
		argv = append(argv, v)
	}
	inv, err := invocation(argv)
	if err != nil {
		return nil, err
	}
	return []Invocation{inv}, nil
}

// nomadInvocations reads `command = "..."` and the string list of the `args = [ ... ]` after it.
func nomadInvocations(s string) ([]Invocation, error) {
	var out []Invocation
	for {
		i := strings.Index(s, "command = \"")
		if i < 0 {
			return out, nil
		}
		s = s[i+len("command = \""):]
		cmd, rest, ok := strings.Cut(s, "\"")
		if !ok {
			return nil, fmt.Errorf("artifacts: unterminated command")
		}
		s = rest
		argv := []string{cmd}
		next := strings.Index(s, "command = \"")
		if a := strings.Index(s, "args = ["); a >= 0 && (next < 0 || a < next) {
			body, rest, ok := strings.Cut(s[a+len("args = ["):], "]")
			if !ok {
				return nil, fmt.Errorf("artifacts: unterminated args")
			}
			s = rest
			// Between quotes are the values; between values, the separators.
			for k, part := range strings.Split(body, "\"") {
				if k%2 == 1 {
					argv = append(argv, part)
				}
			}
		}
		inv, err := invocation(argv)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
}
