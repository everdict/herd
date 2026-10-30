package deploy

import (
	"io"
	"testing"
)

func TestParseFlags(t *testing.T) {
	l, err := parseLabels("host=mac,os=darwin")
	if err != nil || l["host"] != "mac" || l["os"] != "darwin" {
		t.Fatalf("parseLabels = %v, %v", l, err)
	}
	if _, err := parseLabels("nokey"); err == nil {
		t.Error("parseLabels accepted a label without =")
	}
	w, err := parseWarm("tpl-a=2,tpl-b=0")
	if err != nil || len(w) != 2 || w[0].UID != "tpl-a" || w[0].Warm != 2 || len(w[0].Containers) != 0 {
		t.Fatalf("parseWarm = %+v, %v", w, err)
	}
	for _, bad := range []string{"tpl", "tpl=-1", "=2", "tpl=x"} {
		if _, err := parseWarm(bad); err == nil {
			t.Errorf("parseWarm(%q) accepted", bad)
		}
	}
}

// TestHerdletKeepsR2cCommandLines: the command lines R2c's herdlet took (bare `herdlet` with a herdlet credential,
// and `herdlet doctor`) still parse, with every flag it had, and `herdlet run` takes the same.
func TestHerdletKeepsR2cCommandLines(t *testing.T) {
	r2c := []string{"-api", "10.0.0.1:7443", "-router", "10.0.0.1:7444", "-token-file", "/etc/herd/node.jwt", "-worker", "node-1",
		"-labels", "host=ubuntu", "-slots", "4", "-state", "/var/lib/herdlet", "-snapshots", "/srv/herd/snapshots",
		"-kernel", "/usr/share/herd/vmlinux", "-initrd", "/usr/share/herd/initramfs", "-cmdline", "console=hvc0",
		"-vcpus", "2", "-memory-mib", "1024", "-warm", "tpl=2", "-guest-port", "8080", "-cloud-hypervisor", "/usr/local/bin/cloud-hypervisor",
		"-virtiofsd", "/usr/libexec/virtiofsd", "-virtiofsd-args", "--sandbox=none", "-host", "mac-1"}
	for _, args := range [][]string{r2c, append([]string{"run"}, r2c...), append([]string{"doctor"}, r2c...)} {
		if _, err := Parse("herdlet", args, io.Discard); err != nil {
			t.Errorf("herdlet %v: %v", args[:1], err)
		}
	}
}
