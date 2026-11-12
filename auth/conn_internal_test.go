package auth

import (
	"net"
	"testing"
	"time"
)

// TestDrainingByTheWallClock (R2): a connection takes no call once its client's leaf has ended by the wall clock,
// even when the drain timer (the monotonic clock) has not fired.
func TestDrainingByTheWallClock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b := net.Pipe()
	defer b.Close()
	h := &handshake{at: now, notAfter: now.Add(time.Minute), raw: a, now: func() time.Time { return now }}
	if !h.begin() {
		t.Fatal("a call before the leaf ends was refused")
	}
	h.end()
	now = now.Add(2 * time.Minute)
	if h.begin() {
		t.Error("a call after the leaf ended by the wall clock was taken")
	}
}
