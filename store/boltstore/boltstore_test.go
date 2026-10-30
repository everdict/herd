package boltstore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/boltstore"
	"github.com/everdict/herd/store/storetest"
)

func open(t *testing.T, path string) *boltstore.Store {
	t.Helper()
	s, err := boltstore.Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "herd.db"))
	defer s.Close()
	storetest.Run(t, s)
}

func TestDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herd.db")
	storetest.RunReopen(t, func(t *testing.T) store.Store { return open(t, path) })
}

// One herd-api per file: a second open waits for the lock and gives up.
func TestOneProcessPerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herd.db")
	s := open(t, path)
	defer s.Close()
	if s2, err := boltstore.Open(path, 50*time.Millisecond); err == nil {
		s2.Close()
		t.Fatal("a second Open of a locked file succeeded")
	}
}
