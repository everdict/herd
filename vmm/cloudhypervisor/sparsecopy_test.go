//go:build linux

package cloudhypervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestImportWritesZerosAsHoles (PLT-591): an imported memory file reads back byte for byte, ends at its size even when
// it ends in zeros, and its zero blocks take no disk.
func TestImportWritesZerosAsHoles(t *testing.T) {
	data := make([]byte, 8<<20+100)
	copy(data[1<<20:], bytes.Repeat([]byte("guest"), 1000))
	data[4<<20+7] = 1
	p := filepath.Join(t.TempDir(), "memory-ranges")
	if err := writeFrom(p, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the imported file reads back %d bytes (%v), want its %d bytes", len(got), err, len(data))
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if used := fi.Sys().(*syscall.Stat_t).Blocks * 512; used > 1<<20 {
		t.Errorf("the imported file takes %d bytes of disk for 2 blocks of data", used)
	}
	if err := writeFrom(p+"-short", bytes.NewReader(data[:100]), int64(len(data))); err == nil {
		t.Errorf("an import of fewer bytes than the file's size succeeded")
	}
}
