package artifacts_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// TestMacHerdletJoinsLikeLinux (criterion launchd-install): the launchd daemon a Mac installs runs the same command
// as the systemd unit on Linux — `herdlet join <herd-api>` with the join token file, the state directory and the
// guest kernel and initramfs — and no flag the Linux unit does not pass; only paths differ. It is a launchd daemon
// that starts at load and is kept alive, Interactive, with room for a host of VMs' files.
func TestMacHerdletJoinsLikeLinux(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	inv := map[artifacts.Kind]artifacts.Invocation{}
	var plist []byte
	for _, f := range files {
		if f.Path != "launchd/com.everdict.herdlet.plist" && f.Path != "systemd/herdlet.service" {
			continue
		}
		is, err := artifacts.Invocations(f)
		if err != nil || len(is) != 1 {
			t.Fatalf("%s: %v %v", f.Path, is, err)
		}
		inv[f.Kind] = is[0]
		if f.Kind == artifacts.Launchd {
			plist = f.Data
		}
	}
	mac, linux := inv[artifacts.Launchd], inv[artifacts.Systemd]
	flags := func(i artifacts.Invocation) (string, map[string]bool) {
		fs := map[string]bool{}
		for _, a := range i.Args {
			if strings.HasPrefix(a, "-") {
				fs[strings.TrimLeft(a, "-")] = true
			}
		}
		if len(i.Args) == 0 {
			return "", fs
		}
		return i.Args[0], fs
	}
	ms, mf := flags(mac)
	ls, lf := flags(linux)
	if mac.Binary != "herdlet" || linux.Binary != "herdlet" || ms != "join" || ls != "join" {
		t.Fatalf("Mac runs %s %q, Linux %s %q; want both herdlet join", mac.Binary, mac.Args, linux.Binary, linux.Args)
	}
	for _, f := range []string{"token-file", "state", "kernel", "initrd"} {
		if !mf[f] || !lf[f] {
			t.Errorf("-%s: Mac %v, Linux %v; both joins pass it", f, mf[f], lf[f])
		}
	}
	for f := range mf {
		// -secrets is the in-process herd-egress's on a Mac (ADR A-21); a Linux host gives it to herd-egress's own unit.
		if !lf[f] && f != "secrets" {
			t.Errorf("the Mac join passes -%s, which the Linux join does not: the paths differ, the join must not", f)
		}
	}
	for _, want := range []string{"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>", "<string>Interactive</string>", "<key>NumberOfFiles</key>"} {
		if !bytes.Contains(plist, []byte(want)) {
			t.Errorf("herdlet's launchd daemon lacks %q", want)
		}
	}
}

// TestJoinedHerdletsPassNoSnapshotDir (criterion units): no artifact runs `herdlet join` with -snapshots, so a joined
// herdlet keeps snapshots in the store its join hands it, and every herd-api a join mode runs (systemd, Nomad) is
// given -s3 to hand one out. Its counterexample is the R2c systemd unit and Nomad job, whose herdlets wrote snapshots
// to /var/lib/herd/snapshots on their own host, where herd-api could not verify them.
func TestJoinedHerdletsPassNoSnapshotDir(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	joins, apis := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		for _, inv := range invs {
			switch {
			case inv.Binary == "herdlet" && len(inv.Args) > 0 && inv.Args[0] == "join":
				joins[f.Path] = true
				for _, a := range inv.Args {
					if n := strings.TrimLeft(a, "-"); a != n && (n == "snapshots" || strings.HasPrefix(n, "snapshots=")) {
						t.Errorf("%s runs herdlet join with %s: a joined herdlet's snapshots go to the join's store", f.Path, a)
					}
				}
			case inv.Binary == "herd-api":
				apis[f.Path] = slices.Contains(inv.Args, "-s3")
			}
		}
	}
	for _, p := range []string{"systemd/herdlet.service", "launchd/com.everdict.herdlet.plist", "nomad/herdlet.nomad.hcl"} {
		if !joins[p] {
			t.Errorf("%s runs no herdlet join", p)
		}
	}
	for _, p := range []string{"systemd/herd-api.service", "nomad/herd-api.nomad.hcl"} {
		if !apis[p] {
			t.Errorf("%s does not give herd-api -s3: its joined herdlets would have no snapshot store to share", p)
		}
	}
}

// TestMacPlistsPassSecrets (criterion docs, PLT-209): both launchd daemons a Mac installs, the joined herdlet and the
// all-in-one, run with -secrets naming /usr/local/etc/herd/secrets, the directory the Mac's in-process herd-egress
// resolves ate-secret://dir/<name>/<tail> credentials from; without it every credential header is refused there.
func TestMacPlistsPassSecrets(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"launchd/com.everdict.herdlet.plist": {"join", "@HERD_API@", "-token-file", "/usr/local/etc/herd/join.token",
			"-state", "/usr/local/var/herdlet", "-kernel", "/usr/local/share/herd/kernel",
			"-initrd", "/usr/local/share/herd/initrd.img", "-secrets", "/usr/local/etc/herd/secrets"},
		"launchd/com.everdict.herd.plist": {"-data", "/usr/local/var/herd", "-listen", "0.0.0.0:7443",
			"-router-listen", "0.0.0.0:8443", "-kernel", "/usr/local/share/herd/kernel",
			"-initrd", "/usr/local/share/herd/initrd.img", "-secrets", "/usr/local/etc/herd/secrets"},
	}
	for _, f := range files {
		args, ok := want[f.Path]
		if !ok {
			continue
		}
		delete(want, f.Path)
		invs, err := artifacts.Invocations(f)
		if err != nil || len(invs) != 1 {
			t.Fatalf("%s: %v %v", f.Path, invs, err)
		}
		if !slices.Equal(invs[0].Args, args) {
			t.Errorf("%s runs %s %q, want %q", f.Path, invs[0].Binary, invs[0].Args, args)
		}
		if _, err := deploy.Parse(invs[0].Binary, invs[0].Args, io.Discard); err != nil {
			t.Errorf("%s: %v", f.Path, err)
		}
	}
	for p := range want {
		t.Errorf("%s is not rendered", p)
	}
}

// TestLinuxArtifactsUseTheArchivesVirtiofsd (criterion bundled): every artifact that runs a Linux herdlet (the
// systemd units of the all-in-one and of a joined host, and the Nomad herdlet job) points -virtiofsd at the virtiofsd
// the release archive carries: where the runbooks install it (deploy.DefaultVirtiofsd), or the Nomad task directory
// the archive is unpacked into. The units that named /usr/libexec/virtiofsd, a distribution's (1.10 on Ubuntu 24.04):
// red.
func TestLinuxArtifactsUseTheArchivesVirtiofsd(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"systemd/herd.service":    deploy.DefaultVirtiofsd,
		"systemd/herdlet.service": deploy.DefaultVirtiofsd,
		"nomad/herdlet.nomad.hcl": "${NOMAD_TASK_DIR}/virtiofsd",
	}
	got := map[string]string{}
	for _, f := range files {
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		for _, inv := range invs {
			if i := slices.Index(inv.Args, "-virtiofsd"); i >= 0 && i+1 < len(inv.Args) {
				got[f.Path] = inv.Args[i+1]
			}
		}
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s runs -virtiofsd %q, want the release archive's %s", p, got[p], w)
		}
	}
}
