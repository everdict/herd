// Package runbooks_test checks the runbooks against the binaries: every herd command line in a runbook's shell blocks
// must parse with that binary's own flag set (deploy.Parse, what its main runs), and every dist/ artifact a runbook
// names must exist.
package runbooks_test

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
