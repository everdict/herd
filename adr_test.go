package herd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// specDecisions are the decisions of the herd spec (docs/specs/architecture/herd.md rev 2/3) that ADR 0001 must record,
// each with words its section has to contain so that a heading alone does not satisfy it.
var specDecisions = []struct {
	id    string
	words []string
}{
	{"R-D1", []string{"separate product", "no cluster tool"}},
	{"R-D2", []string{"Substrate", "Control", "capability extension"}},
	{"R-D3", []string{"herd-api", "herdlet", "herd-router", "herd-egress", "herd-guest"}},
	{"R-D4", []string{"all-in-one", "herdlet join", "Nomad", "one code path"}},
	{"R-D5", []string{"Go"}},
	{"R-D6", []string{"gone", "present", "unknown", "durable before release", "no credential"}},
	{"R-D7", []string{"microVM", "containers", "volumes", "herdlet doctor"}},
	{"R-D8", []string{"Cloud Hypervisor", "virtiofsd", "Virtualization.framework", "vsock", "arm64", "amd64"}},
	{"R-D9", []string{"suspendMaxFidelity", "restoreFamily", "MEMORY", "VOLUMES", "microvm-ch-", "microvm-vz-arm64-", "microvm-volumes"}},
	{"R-D10", []string{"Linux guests only", "Seatbelt"}},
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// sections splits a markdown document into its "### <id> ..." sections, keyed by id.
func sections(doc string) map[string]string {
	out := map[string]string{}
	heading := regexp.MustCompile(`(?m)^### ([A-Z]+-[A-Z]*\d+)\b.*$`)
	locs := heading.FindAllStringSubmatchIndex(doc, -1)
	for i, loc := range locs {
		end := len(doc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if next := strings.Index(doc[loc[1]:end], "\n## "); next >= 0 {
			end = loc[1] + next
		}
		out[doc[loc[2]:loc[3]]] = doc[loc[1]:end]
	}
	return out
}

func TestADRRecordsEverySpecDecision(t *testing.T) {
	secs := sections(readFile(t, "docs/adr/0001-herd.md"))
	for _, d := range specDecisions {
		sec, ok := secs[d.id]
		if !ok {
			t.Errorf("ADR 0001 has no section for %s", d.id)
			continue
		}
		if !strings.Contains(sec, "**Decision.**") {
			t.Errorf("ADR 0001 %s states no **Decision.**", d.id)
		}
		for _, w := range d.words {
			if !strings.Contains(sec, w) {
				t.Errorf("ADR 0001 %s does not mention %q", d.id, w)
			}
		}
	}
}

// Every decision the ADR makes on its own account (A-n) is stated, not implied.
func TestADRStatesEveryOwnDecision(t *testing.T) {
	secs := sections(readFile(t, "docs/adr/0001-herd.md"))
	own := 0
	for id, sec := range secs {
		if !strings.HasPrefix(id, "A-") {
			continue
		}
		own++
		if !strings.Contains(sec, "**Decision.**") {
			t.Errorf("ADR 0001 %s states no **Decision.**", id)
		}
	}
	if own == 0 {
		t.Errorf("ADR 0001 records no decisions of its own (A-n): name, module path, license, API pin")
	}
}

func TestReadmeSaysWhatHerdIsAndIsNot(t *testing.T) {
	readme := readFile(t, "README.md")
	for _, h := range []string{"## What herd is\n", "## What herd is not\n"} {
		if !strings.Contains(readme, h) {
			t.Errorf("README.md lacks the section %q", strings.TrimSpace(h))
		}
	}
}

func TestModuleAndLicense(t *testing.T) {
	if mod := readFile(t, "go.mod"); !strings.HasPrefix(mod, "module github.com/everdict/herd\n") {
		t.Errorf("go.mod does not declare module github.com/everdict/herd")
	}
	lic := readFile(t, "LICENSE")
	if !strings.Contains(lic, "Apache License") || !strings.Contains(lic, "Version 2.0") {
		t.Errorf("LICENSE is not Apache-2.0")
	}
}
