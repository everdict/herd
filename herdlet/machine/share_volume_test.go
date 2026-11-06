package machine

import (
	"os"
	"testing"

	"github.com/everdict/herd/guest/initd"
)

// A DurableDir volume is the box's workspace, and an image may run as any user: the first-party session image runs as
// uid 1001, and every write it tried failed with "Permission denied" on a directory herd made 0755 for root. A volume
// is shared the way a Kubernetes emptyDir is, whatever the container's user and whatever the host's umask.
func TestLayVolumesLeavesAVolumeWritableByAnyUser(t *testing.T) {
	old := umask(0o077)
	defer umask(old)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := layVolumes(root, &initd.Plan{Volumes: []initd.Volume{{Name: "workspace"}}}); err != nil {
		t.Fatal(err)
	}
	fi, err := root.Lstat("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o777 {
		t.Fatalf("volume mode = %o, want 777", got)
	}
}
