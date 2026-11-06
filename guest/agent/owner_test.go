package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A created file and the parents it needed belong to the container's user: a delegate that cannot edit its seeded files,
// or rm one in the sticky /tmp, fails later and far away. An existing path keeps its owner.
func TestOwnCreatedChownsOnlyWhatItCreated(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "have"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	created := missingParents(root, "have/a/b")
	if err := root.MkdirAll("have/a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	var got []string
	chown := func(name string) error { got = append(got, name); return nil }
	if err := ownAll(append(created, "have/a/b/f"), chown); err != nil {
		t.Fatal(err)
	}
	if want := []string{"have/a", "have/a/b", "have/a/b/f"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chowned %v, want %v", got, want)
	}
}
