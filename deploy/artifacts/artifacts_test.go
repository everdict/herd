package artifacts_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
			r, err := deploy.Parse(inv.Binary, withVarDefaults(f, inv.Args), io.Discard)
			if err != nil {
				t.Errorf("%s: %s %s: %v", f.Path, inv.Binary, strings.Join(inv.Args, " "), err)
				continue
			}
			seen[string(f.Kind)+":"+r.Command.Name()] = true
		}
	}
	for _, k := range []string{
		"systemd:herd", "systemd:herd-api", "systemd:herd-router", "systemd:herdlet run", "systemd:herd-egress",
		"launchd:herd", "launchd:herdlet run",
		"nomad:herd-api", "nomad:herd-router", "nomad:herdlet run", "nomad:herd-egress",
	} {
		if !seen[k] {
			t.Errorf("no %s artifact runs %s", strings.SplitN(k, ":", 2)[0], strings.SplitN(k, ":", 2)[1])
		}
	}
}

// withVarDefaults is args as a Nomad job runs them with its variables' defaults: ${var.<name>} replaced.
func withVarDefaults(f artifacts.File, args []string) []string {
	if f.Kind != artifacts.Nomad {
		return args
	}
	out := slices.Clone(args)
	for _, m := range regexp.MustCompile(`(?s)variable "([a-z_]+)" \{\s*type\s*=\s*string\s*default\s*=\s*"([^"]*)"`).FindAllSubmatch(f.Data, -1) {
		for i := range out {
			out[i] = strings.ReplaceAll(out[i], "${var."+string(m[1])+"}", string(m[2]))
		}
	}
	return out
}

// TestNomadJobPair: herd-api is a service job and herdlet a system job; the herdlet job reads its Nomad variable
// (registry logins) and finds herd-api by its Nomad service, and herd-api mounts the SPIRE agent's socket.
func TestNomadJobPair(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"nomad/herd-api.nomad.hcl", `type = "service"`},
		{"nomad/herdlet.nomad.hcl", `type = "system"`},
		{"nomad/herdlet.nomad.hcl", `nomadVar \"nomad/jobs/herdlet\"`},
		{"nomad/herdlet.nomad.hcl", `nomadService \"herd-api\"`},
		{"nomad/herd-api.nomad.hcl", `source    = "spire-agent-socket"`},
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
// as the systemd unit on Linux — `herdlet run -api <herd-api>` as its SPIFFE identity, with the state directory and the
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
	if mac.Binary != "herdlet" || linux.Binary != "herdlet" || ms != "run" || ls != "run" {
		t.Fatalf("Mac runs %s %q, Linux %s %q; want both herdlet run", mac.Binary, mac.Args, linux.Binary, linux.Args)
	}
	for _, f := range []string{"state", "kernel", "initrd"} {
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
			case inv.Binary == "herdlet" && len(inv.Args) > 0 && inv.Args[0] == "run":
				joins[f.Path] = true
				for _, a := range inv.Args {
					if n := strings.TrimLeft(a, "-"); a != n && (n == "snapshots" || strings.HasPrefix(n, "snapshots=")) {
						t.Errorf("%s runs herdlet run with %s: a joined herdlet's snapshots go to the join's store", f.Path, a)
					}
				}
			case inv.Binary == "herd-api":
				apis[f.Path] = slices.Contains(inv.Args, "-s3")
			}
		}
	}
	for _, p := range []string{"systemd/herdlet.service", "launchd/com.everdict.herdlet.plist", "nomad/herdlet.nomad.hcl"} {
		if !joins[p] {
			t.Errorf("%s runs no herdlet run", p)
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
		"launchd/com.everdict.herdlet.plist": {"run", "-api", "@HERD_API@",
			"-state", "/usr/local/var/herdlet", "-kernel", "/usr/local/share/herd/kernel",
			"-initrd", "/usr/local/share/herd/initrd.img", "-secrets", "/usr/local/etc/herd/secrets",
			"-registry-auth", "/usr/local/etc/herd/registry-auth.json"},
		"launchd/com.everdict.herd.plist": {"-data", "/usr/local/var/herd", "-listen", "0.0.0.0:7443",
			"-router-listen", "0.0.0.0:8443", "-kernel", "/usr/local/share/herd/kernel",
			"-initrd", "/usr/local/share/herd/initrd.img", "-secrets", "/usr/local/etc/herd/secrets",
			"-registry-auth", "/usr/local/etc/herd/registry-auth.json"},
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

// TestHerdletsVMsAreNotTheirTasksMemory (PLT-214): under Nomad raw_exec every process the herdlet starts was in its
// task's cgroup, and a task that declares no resources gets Nomad's default 300 MB, which one MEMORY restore exceeds.
// The herdlet task declares resources for the herdlet itself (memory above that default), and no Linux artifact that
// runs a herdlet (the Nomad task, the herdlet and all-in-one units) keeps its VMs in its own cgroup (-vm-cgroup ""),
// or limits that cgroup with MemoryMax. The job with no resources block: red.
func TestHerdletsVMsAreNotTheirTasksMemory(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]artifacts.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	job := string(byPath["nomad/herdlet.nomad.hcl"].Data)
	_, task, _ := strings.Cut(job, `task "herdlet" {`)
	task, _, _ = strings.Cut(task, "\n    task ")
	_, res, ok := strings.Cut(task, "resources {")
	if !ok {
		t.Fatalf("the herdlet task declares no resources, so Nomad limits it (and, before PLT-214, its VMs) to its default 300 MB:\n%s", task)
	}
	res, _, _ = strings.Cut(res, "}")
	mem := regexp.MustCompile(`(?m)^\s*memory\s*=\s*(\d+)\s*$`).FindStringSubmatch(res)
	if mem == nil {
		t.Fatalf("the herdlet task's resources declare no memory: %s", res)
	}
	if n, _ := strconv.Atoi(mem[1]); n <= 300 {
		t.Errorf("the herdlet task declares memory = %d MB, no more than Nomad's default 300", n)
	}
	for _, p := range []string{"nomad/herdlet.nomad.hcl", "systemd/herdlet.service", "systemd/herd.service"} {
		f := byPath[p]
		if strings.Contains(string(f.Data), "MemoryMax=") {
			t.Errorf("%s sets MemoryMax", p)
		}
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, inv := range invs {
			for i, a := range inv.Args {
				if (a == "-vm-cgroup" && i+1 < len(inv.Args) && inv.Args[i+1] == "") || a == "-vm-cgroup=" {
					t.Errorf("%s keeps its VMs in the herdlet's own cgroup", p)
				}
			}
		}
	}
}

// TestEveryHerdletPassesRegistryAuth (criterion private-pull): every artifact that runs a herdlet (herdlet join, herd
// all-in-one; systemd, launchd, Nomad) passes -registry-auth, so a node's private template images pull with its
// registry logins once the file is there; the Nomad job renders the file from its variable's registry_auth, 0600.
func TestEveryHerdletPassesRegistryAuth(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		for _, inv := range invs {
			if inv.Binary != "herd" && inv.Binary != "herdlet" {
				continue
			}
			seen[f.Path] = true
			if i := slices.Index(inv.Args, "-registry-auth"); i < 0 || i+1 >= len(inv.Args) || inv.Args[i+1] == "" {
				t.Errorf("%s runs %s without -registry-auth: %q", f.Path, inv.Binary, inv.Args)
			}
		}
		if f.Path == "nomad/herdlet.nomad.hcl" {
			tmpl := regexp.MustCompile(`(?s)template \{\s*data\s*=\s*"[^\n]*registry_auth[^\n]*"\s*destination\s*=\s*"secrets/registry-auth.json"\s*perms\s*=\s*"0600"`)
			if !tmpl.Match(f.Data) {
				t.Errorf("%s does not render secrets/registry-auth.json 0600 from the variable's registry_auth", f.Path)
			}
		}
	}
	for _, p := range []string{"systemd/herd.service", "systemd/herdlet.service", "launchd/com.everdict.herd.plist", "launchd/com.everdict.herdlet.plist", "nomad/herdlet.nomad.hcl"} {
		if !seen[p] {
			t.Errorf("%s runs no herdlet", p)
		}
	}
}

// TestArtifactsNameNoRegistryAddress (DEFAUL-1600, criterion a-pull-names-its-plain-hosts): a template's golden
// pull names its own plain-HTTP hosts (image_plain_http_hosts), so the Nomad herdlet job's insecure_registries, the
// node-level fallback it still passes as -insecure-registry, defaults to none, as artifacts.Defaults does; and no
// rendered artifact nor file under dist/ holds an IPv4 address other than loopback or the unspecified one: no job
// names a cluster's address.
func TestArtifactsNameNoRegistryAddress(t *testing.T) {
	if artifacts.Defaults.InsecureRegistries != "" {
		t.Errorf("artifacts.Defaults.InsecureRegistries = %q, want none", artifacts.Defaults.InsecureRegistries)
	}
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	addr := regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	allowed := map[string]bool{"127.0.0.1": true, "0.0.0.0": true}
	scan := func(what string, b []byte) {
		for _, a := range addr.FindAll(b, -1) {
			if !allowed[string(a)] {
				t.Errorf("%s holds the address %s", what, a)
			}
		}
	}
	runs := false
	for _, f := range files {
		scan("rendered "+f.Path, f.Data)
		if f.Path != "nomad/herdlet.nomad.hcl" {
			continue
		}
		v := regexp.MustCompile(`(?s)variable "insecure_registries" \{\s*type\s*=\s*string\s*default\s*=\s*"([^"]*)"`).FindSubmatch(f.Data)
		if v == nil || string(v[1]) != "" {
			t.Errorf("%s: variable insecure_registries = %q, want a string defaulting to none", f.Path, v)
		}
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, inv := range invs {
			if inv.Binary != "herdlet" {
				continue
			}
			runs = true
			if i := slices.Index(inv.Args, "-insecure-registry"); i < 0 || i+1 >= len(inv.Args) || inv.Args[i+1] != "${var.insecure_registries}" {
				t.Errorf("%s: herdlet %q does not pass -insecure-registry ${var.insecure_registries}", f.Path, inv.Args)
			}
		}
	}
	if !runs {
		t.Error("nomad/herdlet.nomad.hcl runs no herdlet")
	}
	n := 0
	if err := filepath.WalkDir(dist, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		scan(p, b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n < len(files) {
		t.Fatalf("read %d files under dist/, fewer than the %d rendered: the scan did not read the tree", n, len(files))
	}
}

// TestNomadEgressDeclaresHostServicesByDiscovery (DEFAUL-1599): the Nomad herd-egress task takes -host-services-file
// from a template that Nomad's service discovery renders (empty when no service is tagged), which herd-egress re-reads
// itself, so no address is an input of the job; its -router comes from the registered herd-router service.
func TestNomadEgressDeclaresHostServicesByDiscovery(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path != "nomad/herdlet.nomad.hcl" {
			continue
		}
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		for _, inv := range invs {
			if inv.Binary == "herd-egress" {
				args = inv.Args
			}
		}
		i := slices.Index(args, "-host-services-file")
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("herd-egress %q takes no -host-services-file", args)
		}
		if j := slices.Index(args, "-router"); j < 0 || j+1 >= len(args) || args[j+1] != "${HERD_ROUTER}" {
			t.Errorf("herd-egress %q takes no -router ${HERD_ROUTER}", args)
		}
		m := regexp.MustCompile(`(?s)template \{\s*data\s*=\s*"([^\n]*)"\s*destination\s*=\s*"` + regexp.QuoteMeta(args[i+1]) + `"\s*change_mode\s*=\s*"noop"`).FindSubmatch(f.Data)
		if m == nil {
			t.Fatalf("no template renders %s with change_mode noop", args[i+1])
		}
		data := string(m[1])
		if !strings.Contains(data, "nomadServices") || !strings.Contains(data, "nomadService .Name") || !strings.Contains(data, `\"herd-host-service\"`) {
			t.Errorf("the host services template is not rendered from Nomad service discovery: %s", data)
		}
		if regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`).MatchString(data) {
			t.Errorf("the host services template holds an address: %s", data)
		}
		if !regexp.MustCompile(`HERD_ROUTER=\{\{ range [^"]*nomadService \\"herd-router\\"`).Match(f.Data) {
			t.Errorf("HERD_ROUTER is not rendered from the herd-router service")
		}
		return
	}
	t.Error("no nomad/herdlet.nomad.hcl")
}

// TestNoArtifactSetsTheCallTimeout (DEFAUL-1601): no shipped herd-api passes -call-timeout. A snapshot's call waits
// for its size and a restore's for its object's, so -call-timeout is the bound of a call that moves no bytes, and its
// default fits that; the Nomad job's 10m was set by hand for a 4 GiB golden.
func TestNoArtifactSetsTheCallTimeout(t *testing.T) {
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	apis := 0
	for _, f := range files {
		invs, err := artifacts.Invocations(f)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		for _, inv := range invs {
			if inv.Binary != "herd-api" {
				continue
			}
			apis++
			for _, a := range inv.Args {
				if n := strings.TrimLeft(a, "-"); a != n && (n == "call-timeout" || strings.HasPrefix(n, "call-timeout=")) {
					t.Errorf("%s runs herd-api with %s", f.Path, a)
				}
			}
		}
	}
	if apis == 0 {
		t.Fatal("no artifact runs herd-api: the test read nothing")
	}
}
