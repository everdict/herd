package herdlet

import (
	"context"
	"time"
)

// WakeDetector notices that the host slept. The monotonic clock stops while macOS (and Linux) sleep and the wall
// clock does not, so when the wall clock moved more than Slack past the monotonic clock between two ticks, the host
// slept in between. It needs no power-management API, so it works the same in a launchd daemon and in a test.
type WakeDetector struct {
	// Tick is how often the clocks are compared; zero means 5 s.
	Tick time.Duration
	// Slack is how far the clocks may drift apart in one tick without counting as a sleep; zero means 3 s.
	Slack time.Duration
	// Clocks reads the wall clock and the monotonic clock; nil reads the system's.
	Clocks func() (wall time.Time, mono time.Duration)
	// Ticks, if set, replaces the ticker (tests).
	Ticks <-chan time.Time
}

// Run returns a channel that receives once after each sleep, until ctx ends. A wake nobody has taken yet is not
// queued twice.
func (d WakeDetector) Run(ctx context.Context) <-chan struct{} {
	tick, slack, clocks := d.Tick, d.Slack, d.Clocks
	if tick <= 0 {
		tick = 5 * time.Second
	}
	if slack <= 0 {
		slack = 3 * time.Second
	}
	if clocks == nil {
		start := time.Now()
		clocks = func() (time.Time, time.Duration) {
			now := time.Now()
			// Round(0) drops the monotonic reading, so Sub on it is wall-clock time; now.Sub(start) is monotonic.
			return now.Round(0), now.Sub(start)
		}
	}
	ticks := d.Ticks
	var stop func()
	if ticks == nil {
		t := time.NewTicker(tick)
		ticks, stop = t.C, t.Stop
	}
	woke := make(chan struct{}, 1)
	go func() {
		if stop != nil {
			defer stop()
		}
		wall, mono := clocks()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
			}
			w, m := clocks()
			if w.Sub(wall)-(m-mono) > slack {
				select {
				case woke <- struct{}{}:
				default:
				}
			}
			wall, mono = w, m
		}
	}()
	return woke
}
