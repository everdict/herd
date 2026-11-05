// Package runbooks_test checks the runbooks against the binaries: every herd command line in a runbook's shell blocks
// must parse with that binary's own flag set (deploy.Parse, what its main runs), and every dist/ artifact a runbook
// names must exist.
package runbooks_test

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/everdict/herd/deploy"
)

var runbooks = []string{"all-in-one.md", "join.md", "nomad.md", "../conformance.md"}

var binaries = map[string]bool{"herd": true, "herd-api": true, "herd-router": true, "herd-egress": true, "herdlet": true}

// shellBlocks returns the logical lines of a markdown file's ```sh blocks, continuation lines joined.
func shellBlocks(md string) []string {
	var out []string
	in := false
	cur := ""
	for _, l := range strings.Split(md, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			in = !in && t == "```sh"
			continue
		}
		if !in {
			continue
		}
		if c, ok := strings.CutSuffix(t, "\\"); ok {
			cur += c + " "
			continue
		}
		out = append(out, cur+t)
		cur = ""
	}
	return out
}

// commands splits a shell line into simple commands (at |, &&, ||, ;, $( and )) of words. Quotes group words, and
// a <placeholder> is one word.
func commands(line string) [][]string {
	var cmds [][]string
	var words []string
	var w strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, w.String())
			w.Reset()
			inWord = false
		}
	}
	flushCmd := func() {
		flushWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		words = nil
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '#' && !inWord:
			flushCmd()
			return cmds
		case c == '"' || c == '\'':
			j := strings.IndexByte(line[i+1:], c)
			if j < 0 {
				j = len(line) - i - 1
			}
			w.WriteString(line[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '<' && strings.IndexByte(line[i:], '>') > 0:
			j := strings.IndexByte(line[i:], '>')
			w.WriteString(line[i : i+j+1])
			inWord = true
			i += j
		case c == '$' && i+1 < len(line) && line[i+1] == '(':
			flushWord()
			flushCmd()
			i++
		case c == ')' || c == '|' || c == ';' || c == '&':
			flushCmd()
		case c == '>':
			// A redirect ends the command's words.
			flushCmd()
			return cmds
		case c == ' ' || c == '\t':
			flushWord()
		default:
			w.WriteByte(c)
			inWord = true
		}
	}
	flushCmd()
	return cmds
}

// herdCommand returns the herd binary a simple command runs and its arguments, skipping sudo and VAR= assignments.
func herdCommand(words []string) (string, []string, bool) {
	for len(words) > 0 && (words[0] == "sudo" || strings.HasSuffix(words[0], "=") || regexp.MustCompile(`^[A-Z_]+=`).MatchString(words[0])) {
		words = words[1:]
	}
	if len(words) == 0 || !binaries[path.Base(words[0])] {
		return "", nil, false
	}
	return path.Base(words[0]), words[1:], true
}

var distRE = regexp.MustCompile(`\bdist/[A-Za-z0-9._/-]+[A-Za-z0-9]`)

// TestRunbookCommandsExist: each runbook's herd command lines parse with the binaries' own flag sets, and every dist/
// artifact it names exists.
func TestRunbookCommandsExist(t *testing.T) {
	for _, rb := range runbooks {
		t.Run(rb, func(t *testing.T) {
			b, err := os.ReadFile(rb)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, line := range shellBlocks(string(b)) {
				for _, words := range commands(line) {
					bin, args, ok := herdCommand(words)
					if !ok {
						continue
					}
					n++
					if _, err := deploy.Parse(bin, args, io.Discard); err != nil {
						t.Errorf("%s: `%s %s`: %v", rb, bin, strings.Join(args, " "), err)
					}
				}
			}
			t.Logf("%s: %d herd command lines parsed", rb, n)
			if n == 0 {
				t.Errorf("%s names no herd command", rb)
			}
			for _, p := range distRE.FindAllString(string(b), -1) {
				if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(p))); errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s names %s, which does not exist", rb, p)
				}
			}
		})
	}
}

// TestCommandSplitting pins the splitter on the shapes the runbooks use.
func TestCommandSplitting(t *testing.T) {
	got := commands(`TOKEN=$(sudo herd-api token -key-file /k -sub a -space "team-a=write") # comment`)
	if len(got) != 2 || strings.Join(got[1], " ") != "sudo herd-api token -key-file /k -sub a -space team-a=write" {
		t.Errorf("command substitution: %q", got)
	}
	got = commands(`./herd/herd-api workers -api <herd-api client address>:7443 -token "$TOKEN"`)
	if len(got) != 1 || len(got[0]) != 6 || got[0][3] != "<herd-api client address>:7443" {
		t.Errorf("placeholder: %q", got)
	}
	got = commands(`./herd/herd-api join-token -key-file jwt.key -ttl 24h > join.token`)
	if len(got) != 1 || len(got[0]) != 6 {
		t.Errorf("redirect: %q", got)
	}
}

// TestJoinNeedsNoSharedMount: the join runbook asks no host to mount herd-api's snapshot directory, and no herdlet it
// runs is pointed at one (-snapshots): a joined herdlet uses the store its join hands it.
func TestJoinNeedsNoSharedMount(t *testing.T) {
	b, err := os.ReadFile("join.md")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\bmount (it|the shared)|\bNFS\b|shared filesystem`).Match(b) {
		t.Error("join.md asks hosts to share a snapshot directory")
	}
	for _, line := range shellBlocks(string(b)) {
		for _, words := range commands(line) {
			if bin, args, ok := herdCommand(words); ok && bin == "herdlet" {
				for _, a := range args {
					if a == "-snapshots" || a == "--snapshots" || strings.HasPrefix(a, "-snapshots=") || strings.HasPrefix(a, "--snapshots=") {
						t.Errorf("join.md runs `herdlet %s`", strings.Join(args, " "))
					}
				}
			}
		}
	}
	if !strings.Contains(string(b), "HERD_S3=") {
		t.Error("join.md does not configure herd-api's S3 snapshot store")
	}
}

// virtiofsdArg is the -virtiofsd a herd or herdlet command line passes, or "" for the flag's default.
func virtiofsdArg(args []string) string {
	for i, a := range args {
		n := strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(n, "virtiofsd="); ok && a != n {
			return v
		}
		if n == "virtiofsd" && a != n && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestRunbooksInstallHerdsVirtiofsd (criterion bundled): no runbook installs virtiofsd from a distribution's package
// (Ubuntu 24.04's apt line gives 1.10, which rejects --migration-mode); all-in-one.md, which join.md and nomad.md
// follow, installs the release archive's virtiofsd at herdlet's default -virtiofsd; every herd and herdlet command a
// runbook runs uses that default or the archive's own copy; and the archive carries a virtiofsd >= 1.11, pinned to an
// upstream commit. The runbook that said `apt-get install -y virtiofsd` and `-virtiofsd /usr/libexec/virtiofsd`, with
// an archive that held none: red.
func TestRunbooksInstallHerdsVirtiofsd(t *testing.T) {
	fs := flag.NewFlagSet("herdlet", flag.ContinueOnError)
	(&deploy.Herdlet{}).HostFlags(fs)
	if d := fs.Lookup("virtiofsd").DefValue; d != deploy.DefaultVirtiofsd {
		t.Errorf("herdlet's -virtiofsd defaults to %s, not deploy.DefaultVirtiofsd %s", d, deploy.DefaultVirtiofsd)
	}
	installed := map[string]bool{}
	for _, rb := range runbooks {
		b, err := os.ReadFile(rb)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range shellBlocks(string(b)) {
			for _, words := range commands(line) {
				if bin, args, ok := herdCommand(words); ok {
					if v := virtiofsdArg(args); v != "" && v != deploy.DefaultVirtiofsd && path.Base(path.Dir(v)) != "herd" {
						t.Errorf("%s: `%s %s` uses virtiofsd %s, not the release archive's", rb, bin, strings.Join(args, " "), v)
					}
					continue
				}
				for len(words) > 0 && words[0] == "sudo" {
					words = words[1:]
				}
				if len(words) < 2 {
					continue
				}
				switch words[0] {
				case "apt-get", "apt", "dnf", "yum", "snap", "brew":
					for _, w := range words[1:] {
						if strings.Contains(w, "virtiofsd") {
							t.Errorf("%s: `%s` installs a distribution's virtiofsd, which may be older than 1.11 (Ubuntu 24.04's is 1.10)", rb, strings.Join(words, " "))
						}
					}
				case "install":
					if slices.Contains(words, "herd/virtiofsd") {
						dst := words[len(words)-1]
						if strings.HasSuffix(dst, "/") {
							dst += "virtiofsd"
						}
						installed[rb+":"+dst] = true
					}
				}
			}
		}
	}
	if !installed["all-in-one.md:"+deploy.DefaultVirtiofsd] {
		t.Errorf("all-in-one.md does not install herd/virtiofsd at %s (installs: %v)", deploy.DefaultVirtiofsd, installed)
	}

	env, err := os.ReadFile("../../scripts/virtiofsd/source.env")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, l := range strings.Split(string(env), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(k, "#") {
			pins[k] = v
		}
	}
	var major, minor int
	if _, err := fmt.Sscanf(pins["VIRTIOFSD_VERSION"], "%d.%d", &major, &minor); err != nil || major < 1 || major == 1 && minor < 11 {
		t.Errorf("scripts/virtiofsd/source.env pins virtiofsd %q, herd needs >= 1.11 (%v)", pins["VIRTIOFSD_VERSION"], err)
	}
	if pins["VIRTIOFSD_REF"] != "v"+pins["VIRTIOFSD_VERSION"] || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pins["VIRTIOFSD_COMMIT"]) ||
		!strings.HasPrefix(pins["VIRTIOFSD_REPO"], "https://gitlab.com/virtio-fs/") {
		t.Errorf("scripts/virtiofsd/source.env does not pin an upstream tag to a commit: %v", pins)
	}
	build, err := os.ReadFile("../../scripts/virtiofsd/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"$VIRTIOFSD_COMMIT"`, "cargo build --locked", "+crt-static", "--migration-mode", "sha256sum virtiofsd >virtiofsd.sha256"} {
		if !strings.Contains(string(build), want) {
			t.Errorf("scripts/virtiofsd/build.sh lacks %q", want)
		}
	}
	rel, err := os.ReadFile("../../scripts/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"VIRTIOFSD_DIR:?", "sha256sum -c --status virtiofsd.sha256", `install -m 0755 "$vdir/virtiofsd" "$dir/virtiofsd"`} {
		if !strings.Contains(string(rel), want) {
			t.Errorf("scripts/release.sh lacks %q: the linux archives must carry the checked virtiofsd", want)
		}
	}
}

// archiveFiles are the files a release archive unpacks (scripts/release.sh): the binaries, the guest kernel and
// initramfs and, in the Linux archives, herd's virtiofsd.
var archiveFiles = map[string][]string{
	"linux":  {"herd", "herd-api", "herdlet", "herd-router", "herd-egress", "kernel", "initrd.img", "virtiofsd"},
	"darwin": {"herd", "herd-api", "herdlet", "herd-router", "herd-egress", "kernel", "initrd.img"},
}

// section is one "## " part of a runbook: the commands of one kind of host.
type section struct {
	runbook, title, text string
}

func sections(rb, md string) []section {
	var out []section
	cur := section{runbook: rb}
	for _, l := range strings.Split(md, "\n") {
		if t, ok := strings.CutPrefix(l, "## "); ok {
			out = append(out, cur)
			cur = section{runbook: rb, title: t}
			continue
		}
		cur.text += l + "\n"
	}
	return append(out, cur)
}

func (s section) goos() string {
	if strings.Contains(strings.ToLower(s.title), "mac") {
		return "darwin"
	}
	return "linux"
}

var stepRE = regexp.MustCompile(`(?m)^(\d+)\. `)

// steps is the text of numbered steps from..to of a section.
func (s section) steps(from, to int) string {
	locs := stepRE.FindAllStringSubmatchIndex(s.text, -1)
	var b strings.Builder
	for i, loc := range locs {
		n := 0
		fmt.Sscan(s.text[loc[2]:loc[3]], &n)
		end := len(s.text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if n >= from && n <= to {
			b.WriteString(s.text[loc[0]:end])
		}
	}
	return b.String()
}

// layout is the files a host holds after the shell lines ran: what install(1) put where and what an archive
// unpacked with tar -C.
func layout(goos string, lines []string, files map[string]bool) {
	dirs := map[string]bool{}
	for _, line := range lines {
		for _, words := range commands(line) {
			for len(words) > 0 && words[0] == "sudo" {
				words = words[1:]
			}
			if len(words) == 0 {
				continue
			}
			switch words[0] {
			case "tar":
				for i, w := range words {
					if w == "-C" && i+1 < len(words) {
						for _, f := range archiveFiles[goos] {
							files[path.Join(words[i+1], f)] = true
						}
					}
				}
			case "install":
				var ops []string
				dir := false
				for i := 1; i < len(words); i++ {
					switch w := words[i]; {
					case w == "-d":
						dir = true
					case w == "-m" || w == "-o" || w == "-g":
						i++
					case strings.HasPrefix(w, "-"):
					default:
						ops = append(ops, path.Clean(w))
					}
				}
				if dir {
					for _, d := range ops {
						dirs[d] = true
					}
					continue
				}
				if len(ops) < 2 {
					continue
				}
				dst := ops[len(ops)-1]
				for _, src := range ops[:len(ops)-1] {
					if len(ops) > 2 || dirs[dst] || strings.HasSuffix(words[len(words)-1], "/") {
						files[path.Join(dst, path.Base(src))] = true
					} else {
						files[dst] = true
					}
				}
			}
		}
	}
}

var (
	stepsRefRE = regexp.MustCompile(`all-in-one\.md steps? (\d+)(?:[–-](\d+))?`)
	inlineRE   = regexp.MustCompile("`([^`]*herdlet doctor[^`]*)`")
)

// TestEveryDoctorLinePasses (criterion runbook-doctor): every `herdlet doctor` a runbook runs, in a shell block or
// in its text, passes on a host set up exactly as that runbook says up to there: the files its install and tar lines
// (and the all-in-one steps it refers to, and an unpacked release archive where it says so) put on the host, KVM,
// ip(8) and nft(8). nomad.md's prerequisite `sudo ./herd/herdlet doctor -virtiofsd ./herd/virtiofsd` on a client
// that holds only Cloud Hypervisor and the unpacked archive: the kernel and initrd defaults
// (/usr/local/share/herd/...) are not there, red.
func TestEveryDoctorLinePasses(t *testing.T) {
	var aioUbuntu section
	all := map[string][]section{}
	for _, rb := range runbooks {
		b, err := os.ReadFile(rb)
		if err != nil {
			t.Fatal(err)
		}
		all[rb] = sections(rb, string(b))
		for _, s := range all[rb] {
			if rb == "all-in-one.md" && strings.HasPrefix(s.title, "Ubuntu") {
				aioUbuntu = s
			}
		}
	}
	n := 0
	for _, rb := range runbooks {
		for _, s := range all[rb] {
			goos := s.goos()
			files := map[string]bool{}
			for _, m := range stepsRefRE.FindAllStringSubmatch(s.text, -1) {
				from, to := 0, 0
				fmt.Sscan(m[1], &from)
				to = from
				if m[2] != "" {
					fmt.Sscan(m[2], &to)
				}
				layout(goos, shellBlocks("```sh\n"+strings.Join(shellBlocks(aioUbuntu.steps(from, to)), "\n")+"\n```"), files)
			}
			if strings.Contains(s.text, "unpacked archive") {
				for _, f := range archiveFiles[goos] {
					files[path.Join("herd", f)] = true
				}
			}
			var lines []string
			for _, l := range strings.Split(s.text, "\n") {
				for _, m := range inlineRE.FindAllStringSubmatch(l, -1) {
					if m[1] == "herdlet doctor" {
						continue // the command named, not a command line to run
					}
					lines = append(lines, m[1])
				}
			}
			ordered := append(shellBlocks(s.text), lines...)
			for i, line := range ordered {
				for _, words := range commands(line) {
					bin, args, ok := herdCommand(words)
					if !ok || bin != "herdlet" || len(args) == 0 || args[0] != "doctor" {
						continue
					}
					n++
					// The host as the runbook left it before this line (inline lines: after every block of the section).
					got := map[string]bool{}
					for k := range files {
						got[k] = true
					}
					upTo := ordered[:i]
					if i >= len(ordered)-len(lines) {
						upTo = ordered[:len(ordered)-len(lines)]
					}
					layout(goos, upTo, got)
					if fails := doctorOn(t, goos, got, args[1:]); len(fails) > 0 {
						t.Errorf("%s (%s): `%s` fails on the host the runbook sets up: %v", rb, s.title, line, fails)
					}
				}
			}
		}
	}
	t.Logf("%d doctor lines checked", n)
	if n < 4 {
		t.Errorf("found %d doctor lines in the runbooks, want every one (at least 4)", n)
	}
}

// doctorOn runs deploy.Doctor with the doctor line's flags on a host that holds files (paths relative to the
// directory the runbook works in, or absolute) and the system's ip and nft.
func doctorOn(t *testing.T, goos string, files map[string]bool, args []string) map[string]string {
	t.Helper()
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	h := &deploy.Herdlet{}
	h.RunFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("doctor %v: %v", args, err)
	}
	has := func(p string) bool { return files[path.Clean(p)] }
	pathDirs := []string{"/usr/local/bin", "/usr/bin", "/usr/sbin", "/bin", "/sbin"}
	system := map[string]bool{"/usr/sbin/ip": true, "/usr/sbin/nft": true}
	arch := "amd64"
	if goos == "darwin" {
		arch = "arm64"
	}
	host := deploy.Host{GOOS: goos, GOARCH: arch, VZ: true,
		OpenRW: func(string) error { return nil },
		LookPath: func(name string) (string, error) {
			if strings.Contains(name, "/") {
				if has(name) {
					return name, nil
				}
				return "", os.ErrNotExist
			}
			for _, d := range pathDirs {
				if p := path.Join(d, name); has(p) || system[p] {
					return p, nil
				}
			}
			return "", fmt.Errorf("%s: not in PATH", name)
		},
		Stat: func(p string) error {
			if has(p) {
				return nil
			}
			return os.ErrNotExist
		},
		Help: func(p string) (string, error) {
			if path.Base(p) == "virtiofsd" && has(p) {
				return "--socket-path --shared-dir --cache --migration-mode --readonly --sandbox --seccomp", nil
			}
			return "", fmt.Errorf("%s: not herd's virtiofsd", p)
		},
	}
	fails := map[string]string{}
	for _, c := range deploy.Doctor(h, host) {
		if !c.OK {
			fails[c.Name] = c.Detail
		}
	}
	return fails
}

// everdict/herd is a private repository (owner, 2026-11-05: keep it private where possible), so its release assets
// answer 404 to an unauthenticated download: a runbook step `curl …/releases/latest/download/…`, or a Nomad job whose
// artifact defaults to that URL, cannot be followed by anyone. Every runbook that unpacks a release archive downloads
// it with an authenticated `gh release download --repo everdict/herd`, and neither a runbook nor a dist/ artifact
// names the unauthenticated release URL (PLT-117).
func TestReleaseArchiveDownloadsWorkForAPrivateRepository(t *testing.T) {
	public := "github.com/everdict/herd/releases"
	unpacks := regexp.MustCompile(`tar -C herd -xzf (herd_\S+\.tar\.gz)`)
	n := 0
	for _, rb := range runbooks {
		b, err := os.ReadFile(rb)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if strings.Contains(text, public) {
			t.Errorf("%s names %s: a private repository's release cannot be downloaded without authentication", rb, public)
		}
		lines := shellBlocks(text)
		for i, line := range lines {
			m := unpacks.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			n++
			got := false
			for _, prev := range lines[:i] {
				if strings.Contains(prev, "gh release download") && strings.Contains(prev, "--repo everdict/herd") &&
					strings.Contains(prev, m[1]) {
					got = true
				}
			}
			if !got {
				t.Errorf("%s: `%s` unpacks %s, but no earlier line downloads it with `gh release download --repo everdict/herd --pattern %s`", rb, line, m[1], m[1])
			}
		}
	}
	if n < 5 {
		t.Errorf("found %d archive unpack lines in the runbooks, want every one (at least 5): the check looked at nothing", n)
	}
	dist, err := filepath.Glob("../../dist/*/*")
	if err != nil || len(dist) == 0 {
		t.Fatalf("no dist/ artifacts found (%v): the check would look at nothing", err)
	}
	for _, f := range dist {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), public) {
			t.Errorf("%s names %s: a Nomad artifact cannot authenticate to a private repository's release; it must take the archive URL of a mirror the cluster reaches", f, public)
		}
	}
	t.Logf("%d unpack lines and %d dist artifacts checked", n, len(dist))
}
