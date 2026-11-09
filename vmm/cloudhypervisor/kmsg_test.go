//go:build linux

package cloudhypervisor

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestKmsgLogEndsAtTheEndOfTheBuffer reads a device that is open, empty after its records and pollable, as /dev/kmsg is
// at the end of its buffer: a FIFO whose writer the test holds open. The read must answer the record written to it
// and end there, not park in Go's poller waiting for more (PLT-368).
func TestKmsgLogEndsAtTheEndOfTheBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// O_RDWR opens a FIFO without waiting for a reader and holds a writer open, so the reader never sees EOF.
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rec := "3,1234,5678901,-;Out of memory: Killed process 4242 (cloud-hyperviso)\n"
	if _, err := w.WriteString(rec); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	go func() { done <- kmsgLog(path, 4242) }()
	select {
	case got := <-done:
		want := []string{"Out of memory: Killed process 4242 (cloud-hyperviso)"}
		if !slices.Equal(got, want) {
			t.Fatalf("kmsgLog answered %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kmsgLog did not return within 5s of reading its one record: the read parks at the end of the buffer")
	}
}
