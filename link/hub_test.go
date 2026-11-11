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

// next is the herdlet's next Down, or a failure after a while.
func next(t *testing.T, downs <-chan link.Down) link.Down {
	t.Helper()
	select {
	case d := <-downs:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the herdlet got nothing more within 5 s")
		return link.Down{}
	}
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
	go func() {
		got <- h.CallSized(context.Background(), "w", link.Work{Op: link.OpSnapshot}, timeout, perByte)
	}()
	w := next(t, downs).Work
	if dl := time.UnixMilli(w.DeadlineUnixMilli); dl.After(start.Add(timeout + 50*time.Millisecond)) {
		t.Errorf("the work's first deadline is %v after the call, want its timeout %v", dl.Sub(start), timeout)
	}
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 1000}})
	ext := next(t, downs).Extend
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
	w := next(t, downs).Work
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 1 << 20}})
	cdl, _ := ctx.Deadline()
	if ext := next(t, downs).Extend; ext == nil || time.UnixMilli(ext.DeadlineUnixMilli).After(cdl) {
		t.Errorf("the herdlet got %+v, want its deadline moved to the caller's %v", ext, cdl)
	}
	o = <-got
	if o.Result != link.ResultUnknown || time.Now().Before(cdl) || time.Since(cdl) > 5*timeout {
		t.Errorf("a call whose caller's deadline passed answered %+v %v after it", o, time.Since(cdl))
	}

	// A report of fewer bytes than one before leaves the deadline.
	go func() {
		got <- h.CallSized(context.Background(), "w", link.Work{Op: link.OpSnapshot}, timeout, perByte)
	}()
	w = next(t, downs).Work
	send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: 500}})
	first := next(t, downs).Extend
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

// TestOutcomeNamesItsLinksInstance (PLT-371): the hub tells herd-api which herdlet process a link is (OnLinked once
// it is the worker's link, Instance) and stamps every outcome with it; a herdlet older than Register.Instance links
// and answers as before, with no instance.
func TestOutcomeNamesItsLinksInstance(t *testing.T) {
	for _, inst := range []string{"process-1", ""} {
		h := link.NewHub()
		linked := make(chan link.Register, 1)
		h.OnLinked = func(_ context.Context, r link.Register) {
			if got, ok := h.Instance(r.Worker); !ok || got != inst {
				t.Errorf("Instance(%q) = %q, %v while it links, want %q, true", r.Worker, got, ok, inst)
			}
			linked <- r
		}
		up, down := make(chan []byte, 16), make(chan []byte, 16)
		go func() { _ = h.Serve(context.Background(), pipe{in: up, out: down}) }()
		b, _ := json.Marshal(link.Up{Register: &link.Register{Worker: "w", Instance: inst}})
		up <- b
		if r := <-linked; r.Instance != inst {
			t.Errorf("OnLinked got instance %q, want %q", r.Instance, inst)
		}
		go func() {
			for b := range down {
				var d link.Down
				_ = json.Unmarshal(b, &d)
				if d.Work != nil {
					b, _ := json.Marshal(link.Up{Outcome: &link.Outcome{ID: d.Work.ID, Result: link.ResultDone}})
					up <- b
				}
			}
		}()
		o := h.Call(context.Background(), "w", link.Work{Op: link.OpInspect, VM: "vm"})
		if o.Result != link.ResultDone || o.Instance != inst {
			t.Errorf("outcome %+v, want done from instance %q", o, inst)
		}
		if b, _ := json.Marshal(o); strings.Contains(string(b), "process-1") {
			t.Errorf("the outcome's instance is on the wire: %s", b)
		}
		close(up)
	}
}

// TestRegisteredNamesTheStore (PLT-458): the hub's answer to a registration names herd-api's record store, so the
// herdlet knows under which store's records it starts VMs on this link.
func TestRegisteredNamesTheStore(t *testing.T) {
	h := link.NewHub()
	h.StoreID = "0b3e5a52-7d2c-4c1e-9a55-2f0e8d1c6b7a"
	up, down := make(chan []byte, 4), make(chan []byte, 4)
	go func() { _ = h.Serve(context.Background(), pipe{in: up, out: down}) }()
	defer close(up)
	b, _ := json.Marshal(link.Up{Register: &link.Register{Worker: "w"}})
	up <- b
	select {
	case b := <-down:
		var d link.Down
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		if d.Registered == nil || d.Registered.StoreID != h.StoreID {
			t.Fatalf("first Down = %+v, want Registered naming store %s", d, h.StoreID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Registered within 5 s")
	}
}
