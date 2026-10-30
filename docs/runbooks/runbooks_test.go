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
