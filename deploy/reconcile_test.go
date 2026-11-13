package deploy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestTemplatePassesDoNotWaitForActors is the counterexample of the loop that rebuilt one golden every three minutes
// (PLT-591): while the pass over the actors waits for a herdlet that does not answer, the pass over the templates
// keeps running. One loop running both in turn runs the second only when the first returns.
func TestTemplatePassesDoNotWaitForActors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stuck := make(chan struct{})
	var templates atomic.Int32
	go reconcileEvery(ctx, time.Millisecond, "actors", func(ctx context.Context) (int, error) {
		select {
		case <-stuck:
		case <-ctx.Done():
		}
		return 1, nil
	})
	go reconcileEvery(ctx, time.Millisecond, "templates", func(context.Context) (int, error) {
		templates.Add(1)
		return 0, nil
	})
	for deadline := time.Now().Add(5 * time.Second); templates.Load() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d template passes ran in 5 s while the actor pass waited, want them to go on", templates.Load())
		}
	}
	close(stuck)
}
