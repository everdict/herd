package link_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/link"
)

// pipe is one end of an in-memory link.
type pipe struct {
	in  <-chan []byte
	out chan<- []byte
}

func (p pipe) Send(b []byte) error { p.out <- b; return nil }

func (p pipe) Recv() ([]byte, error) {
	b, ok := <-p.in
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

// herdlet registers on a hub and hands each Down it gets to the test; send writes an Up.
func herdlet(t *testing.T, h *link.Hub) (downs <-chan link.Down, send func(link.Up)) {
	up, down := make(chan []byte, 16), make(chan []byte, 16)
	go func() { _ = h.Serve(context.Background(), pipe{in: up, out: down}) }()
	t.Cleanup(func() { close(up) })
	send = func(u link.Up) {
		b, err := json.Marshal(u)
		if err != nil {
			t.Error(err)
		}
		up <- b
	}
	send(link.Up{Register: &link.Register{Worker: "w"}})
	ds := make(chan link.Down, 16)
	go func() {
		for b := range down {
			var d link.Down
			if err := json.Unmarshal(b, &d); err != nil {
				t.Error(err)
			}
			if d.Registered == nil {
				ds <- d
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); len(h.Workers()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the herdlet never registered")
		}
	}
	return ds, send
}

// perByte is a grow function of 1 ms a byte.
func perByte(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// TestCallSizedWaitsForReportedBytes (DEFAUL-1601): a call that moves bytes waits its timeout plus what the herdlet
// reported it moves, and tells the herdlet its new deadline; the outcome that comes after the plain timeout is the
// call's answer, not unknown.
func TestCallSizedWaitsForReportedBytes(t *testing.T) {
	h := link.NewHub()
	downs, send := herdlet(t, h)
	const timeout = 200 * time.Millisecond
	got := make(chan link.Outcome, 1)
	start := time.Now()
	go func() { got <- h.CallSized(context.Background(), "w", link.Work{Op: link.OpSnapshot}, timeout, perByte) }()
	w := (<-downs).Work
	if dl := time.UnixMilli(w.DeadlineUnixMilli); dl.After(start.Add(timeout + 50*time.Millisecond)) {
		t.Errorf("the work's first deadline is %v after the call, want its timeout %v", dl.Sub(start), timeout)
	}
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 1000}})
	ext := (<-downs).Extend
	if ext == nil || ext.ID != w.ID {
		t.Fatalf("after a report of 1000 bytes the herdlet got %+v, want its work's deadline moved", ext)
	}
	if dl := time.UnixMilli(ext.DeadlineUnixMilli); dl.Before(start.Add(timeout + time.Second - 10*time.Millisecond)) {
		t.Errorf("the moved deadline is %v after the call, want at least the timeout and 1000 bytes at 1 ms each", dl.Sub(start))
	}
	time.Sleep(3 * timeout)
	send(link.Up{Outcome: &link.Outcome{ID: w.ID, Result: link.ResultDone}})
	if o := <-got; o.Result != link.ResultDone {
		t.Errorf("the call answered %+v, want the herdlet's done", o)
	}
}

// TestCallSizedStillEnds: a herdlet that reports nothing gets the plain timeout, a smaller report does not shorten a
// deadline, and the caller's own deadline bounds the call whatever the herdlet reports.
func TestCallSizedStillEnds(t *testing.T) {
	h := link.NewHub()
	downs, send := herdlet(t, h)
	const timeout = 100 * time.Millisecond
	start := time.Now()
	o := h.CallSized(context.Background(), "w", link.Work{Op: link.OpSnapshot}, timeout, perByte)
	if took := time.Since(start); o.Result != link.ResultUnknown || !strings.Contains(o.Reason, "deadline exceeded") || took > 10*timeout {
		t.Errorf("a silent herdlet: %+v after %v, want unknown at the %v timeout", o, took, timeout)
	}
	<-downs

	ctx, cancel := context.WithTimeout(context.Background(), 3*timeout)
	defer cancel()
	got := make(chan link.Outcome, 1)
	go func() { got <- h.CallSized(ctx, "w", link.Work{Op: link.OpSnapshot}, timeout, perByte) }()
	w := (<-downs).Work
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 1 << 20}})
	cdl, _ := ctx.Deadline()
	if ext := (<-downs).Extend; ext == nil || time.UnixMilli(ext.DeadlineUnixMilli).After(cdl) {
		t.Errorf("the herdlet got %+v, want its deadline moved to the caller's %v", ext, cdl)
	}
	o = <-got
	if o.Result != link.ResultUnknown || time.Now().Before(cdl) || time.Since(cdl) > 5*timeout {
		t.Errorf("a call whose caller's deadline passed answered %+v %v after it", o, time.Since(cdl))
	}

	// A report of fewer bytes than one before leaves the deadline.
	go func() { got <- h.CallSized(context.Background(), "w", link.Work{Op: link.OpSnapshot}, timeout, perByte) }()
	w = (<-downs).Work
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 500}})
	first := (<-downs).Extend
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 10}})
	send(link.Up{Outcome: &link.Outcome{ID: w.ID, Result: link.ResultDone}})
	if o := <-got; o.Result != link.ResultDone || first == nil {
		t.Errorf("answered %+v after extend %+v", o, first)
	}
	select {
	case d := <-downs:
		t.Errorf("a smaller report sent %+v", d)
	case <-time.After(50 * time.Millisecond):
	}
}
