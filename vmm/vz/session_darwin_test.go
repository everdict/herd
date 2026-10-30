//go:build darwin

package vz_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/everdict/herd/vmm/vz"
)

// TestIORegOnThisMac reads this Mac's console session the way a herdlet daemon does. Run it once unlocked and once
// with the screen locked (e.g. from SSH after locking) and compare with what the screen shows; -v prints ioreg's raw
// root, which is what testdata/ioreg-*.plist stand for.
func TestIORegOnThisMac(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := vz.IORegSession{}.Console(ctx)
	if err != nil {
		t.Fatalf("console session: %v", err)
	}
	t.Logf("console: %+v (%s); Unlocked=%v", c, c, c.Unlocked())
	if testing.Verbose() {
		out, _ := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-n", "Root", "-d1", "-a").Output()
		t.Logf("ioreg -n Root -d1 -a:\n%s", out)
	}
}
