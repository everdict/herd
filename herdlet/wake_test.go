package herdlet_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/herdlet"
)

// TestWakeDetector: a tick whose wall clock ran ahead of the monotonic clock by more than Slack is a wake; ordinary
// ticks and small drift are not.
func TestWakeDetector(t *testing.T) {
	var mu sync.Mutex
	wall, mono := time.Unix(1_800_000_000, 0), time.Duration(0)
	advance := func(w, m time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		wall, mono = wall.Add(w), mono+m
	}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	woke := herdlet.WakeDetector{Slack: 3 * time.Second, Ticks: ticks, Clocks: func() (time.Time, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return wall, mono
	}}.Run(ctx)
	step := func(w, m time.Duration) bool {
		advance(w, m)
		ticks <- time.Time{}
		// The detector reads the clocks after the tick; a second tick with no time passing flushes the first.
		ticks <- time.Time{}
		select {
		case <-woke:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	if step(5*time.Second, 5*time.Second) {
		t.Errorf("an ordinary tick counted as a wake")
	}
	if step(6*time.Second, 5*time.Second) {
		t.Errorf("one second of drift counted as a wake")
	}
	if !step(2*time.Hour, 5*time.Second) {
		t.Errorf("two hours of wall clock over five seconds of monotonic clock was not a wake")
	}
	if step(5*time.Second, 5*time.Second) {
		t.Errorf("the tick after a wake counted again")
	}
}
