package execrec_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/guest/execrec"
)

func TestScriptQuotesEverything(t *testing.T) {
	m := execrec.Meta{ID: "x1", Cwd: "/tmp", Env: map[string]string{"B": "it's $HOME", "A": "`id`"},
		Command: []string{"printf", "%s|%s|%s\\n", "$A", "it's", "$(id)"}}
	f := filepath.Join(t.TempDir(), "cmd.sh")
	if err := os.WriteFile(f, []byte(execrec.Script(m)), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", f).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "$A|it's|$(id)\n" {
		t.Fatalf("script output %q: arguments were not passed literally", out)
	}
}

func TestRecordRefusesBadIDsAndSymlinkedRoots(t *testing.T) {
	root := t.TempDir()
	s, err := execrec.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"", "../x", "a/b", "."} {
		if err := s.Create(execrec.Meta{ID: id, Command: []string{"true"}}); err == nil {
			t.Errorf("Create(%q) accepted", id)
		}
	}
	// A container that points /everdict at the guest's own files does not get the guest to write there.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "everdict")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(execrec.Meta{ID: "abc", Command: []string{"true"}}); err == nil {
		t.Error("Create through a symlink out of the container root succeeded")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("record written outside the container: %v", ents)
	}
}
