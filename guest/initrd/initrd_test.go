package initrd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/initrd"
)

// TestWriteFixesEveryMember (DEFAUL-1626): every member, a file included, gets the given modification time and the
// mode and owner the list gives it, whatever the time its source file was written; the list's order, symlink
// targets and device numbers are kept, and the archive is the same bytes for the same list and contents.
func TestWriteFixesEveryMember(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "guest")
	if err := os.WriteFile(bin, []byte("guest bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := "# a comment\ndir /dev 0755 0 0\nnod /dev/null 0666 0 0 c 1 3\nfile /init ${GUEST} 0755 0 0\nslink /bin/sh /bin/busybox 0777 0 0\n"
	env := func(k string) (string, bool) { return map[string]string{"GUEST": bin}[k], k == "GUEST" }
	write := func(mtime time.Time) []byte {
		if err := os.Chtimes(bin, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := initrd.Write(&b, strings.NewReader(list), env, 1700000000); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	one, two := write(time.Unix(1, 0)), write(time.Now())
	if !bytes.Equal(one, two) {
		t.Errorf("the archive changed with its input file's mtime")
	}
	if len(one)%512 != 0 {
		t.Errorf("archive of %d bytes, want whole 512-byte blocks", len(one))
	}
	ms, err := initrd.Read(bytes.NewReader(one))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		mode uint32
		data string
	}{{"dev", 0o040755, ""}, {"dev/null", 0o020666, ""}, {"init", 0o100755, "guest bytes"}, {"bin/sh", 0o120777, "/bin/busybox\x00"}}
	if len(ms) != len(want) {
		t.Fatalf("%d members, want %d", len(ms), len(want))
	}
	for i, w := range want {
		m := ms[i]
		if m.Name != w.name || m.Mode != w.mode || string(m.Data) != w.data || m.MTime != 1700000000 || m.UID != 0 || m.GID != 0 {
			t.Errorf("member %d: %s mode %o mtime %d uid %d gid %d data %q, want %s mode %o mtime 1700000000 root %q",
				i, m.Name, m.Mode, m.MTime, m.UID, m.GID, m.Data, w.name, w.mode, w.data)
		}
	}
	if ms[1].RDevMajor != 1 || ms[1].RDevMinor != 3 {
		t.Errorf("dev/null is %d:%d, want 1:3", ms[1].RDevMajor, ms[1].RDevMinor)
	}
}

// TestWriteRefuses: an unset variable, an unknown type or a malformed line is an error, not a short archive.
func TestWriteRefuses(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	for _, list := range []string{"file /init ${GUEST} 0755 0 0\n", "pipe /p 0644 0 0\n", "dir /dev 0755 0\n", "dir /dev 0x755 0 0\n"} {
		if err := initrd.Write(&bytes.Buffer{}, strings.NewReader(list), none, 0); err == nil {
			t.Errorf("%q was written", list)
		}
	}
	if err := initrd.Write(&bytes.Buffer{}, strings.NewReader("dir /d 0755 0 0\n"), none, -1); err == nil {
		t.Errorf("a negative mtime was written")
	}
}
