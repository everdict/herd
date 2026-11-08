package tapnet_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/egress/tapnet"
)

// TestSetWaitsForHerdEgress (bound-before-first-flow, DEFAUL-1622): with herd-egress acknowledging (LoadedPath exists),
// Set answers only once the acknowledgement holds the binding, and fails, naming herd-egress, when it never does.
// Without an acknowledgement file (no herd-egress that acknowledges) Set answers at once.
func TestSetWaitsForHerdEgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress-bindings.json")
	b := &tapnet.Bindings{Path: path}
	e := tapnet.Binding{Space: "s", Name: "box", UID: "u1", Source: "10.88.0.2"}
	if err := b.Set(e); err != nil {
		t.Fatalf("Set with no acknowledgement file: %v", err)
	}

	if err := tapnet.Acknowledge(path, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	e2 := tapnet.Binding{Space: "s", Name: "box2", UID: "u2", Source: "10.88.0.6"}
	acked := make(chan time.Time, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		data, err := os.ReadFile(path)
		if err == nil {
			err = tapnet.Acknowledge(path, data)
		}
		if err != nil {
			t.Error(err)
		}
		acked <- time.Now()
	}()
	if err := b.Set(e2); err != nil {
		t.Fatal(err)
	}
	answered := time.Now()
	if at := <-acked; answered.Before(at) {
		t.Fatalf("Set answered %v before herd-egress acknowledged the binding", at.Sub(answered))
	}

	defer func(d time.Duration) { tapnet.LoadedTimeout = d }(tapnet.LoadedTimeout)
	tapnet.LoadedTimeout = 50 * time.Millisecond
	err := b.Set(tapnet.Binding{Space: "s", Name: "box3", UID: "u3", Source: "10.88.0.10"})
	if err == nil || !strings.Contains(err.Error(), "herd-egress has not loaded") {
		t.Fatalf("Set with a stale acknowledgement: %v, want herd-egress has not loaded it", err)
	}
}
