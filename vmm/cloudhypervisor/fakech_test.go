//go:build linux

package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
)

// fakeHost is a fake process table: "starting" the cloud-hypervisor binary serves an in-process fake of Cloud
// Hypervisor v54's REST API on the --api-socket path; "starting" virtiofsd listens on its --socket-path. Faults are
// injected per API endpoint.
type fakeHost struct {
	t            *testing.T
	chBin, fsBin string

	mu             sync.Mutex
	nextPid        int
	procs          map[int]*fakeProc
	hang           map[string]bool
	unreachable    map[string]bool
	delay          map[string]time.Duration
	ignoreShutdown bool // the VMM answers shutdown, keeps running and survives signals
	fsSurvive      bool // virtiofsd survives signals
	calls          []string
	starts         []string
}

func newFakeHost(t *testing.T) *fakeHost {
	h := &fakeHost{t: t, chBin: "/opt/ch/cloud-hypervisor", fsBin: "/usr/libexec/virtiofsd", nextPid: 1000,
		procs: map[int]*fakeProc{}}
	h.clear()
	t.Cleanup(func() {
		h.mu.Lock()
		h.ignoreShutdown, h.fsSurvive = false, false
		ps := make([]*fakeProc, 0, len(h.procs))
		for _, p := range h.procs {
			ps = append(ps, p)
		}
		h.mu.Unlock()
		for _, p := range ps {
			p.exit()
		}
	})
	return h
}

func (h *fakeHost) clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hang, h.unreachable, h.delay = map[string]bool{}, map[string]bool{}, map[string]time.Duration{}
	h.ignoreShutdown, h.fsSurvive = false, false
}

func (h *fakeHost) record(ep string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, ep)
}

func (h *fakeHost) callCount() (calls, starts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls), len(h.starts)
}

func (h *fakeHost) running() (n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.procs {
		if ok, _ := p.Exited(); !ok {
			n++
		}
	}
	return n
}

// Processes.

type fakeProc struct {
	h      *fakeHost
	pid    int
	kind   string
	marker string
	once   sync.Once
	done   chan struct{}
	stop   func()
}

func (p *fakeProc) Pid() int { return p.pid }

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.h.mu.Lock()
	survive := (p.kind == "ch" && p.h.ignoreShutdown) || (p.kind == "fs" && p.h.fsSurvive)
	p.h.mu.Unlock()
	if !survive {
		p.exit()
	}
	return nil
}

func (p *fakeProc) Exited() (bool, error) {
	select {
	case <-p.done:
		return true, nil
	default:
		return false, nil
	}
}

func (p *fakeProc) exit() {
	p.once.Do(func() {
		p.stop()
		close(p.done)
	})
}

func (h *fakeHost) Start(ps ProcSpec) (Process, error) {
	h.mu.Lock()
	h.nextPid++
	p := &fakeProc{h: h, pid: h.nextPid, marker: ps.Marker, done: make(chan struct{})}
	h.starts = append(h.starts, ps.Path)
	h.mu.Unlock()
	switch ps.Path {
	case h.chBin:
		var sock string
		for i, a := range ps.Args {
			if a == "--api-socket" && i+1 < len(ps.Args) {
				sock = strings.TrimPrefix(ps.Args[i+1], "path=")
			}
		}
		l, err := net.Listen("unix", sock)
		if err != nil {
			return nil, err
		}
		p.kind = "ch"
		ch := &fakeCH{h: h, proc: p, state: "none"}
		srv := &http.Server{Handler: ch}
		go func() { _ = srv.Serve(l) }()
		p.stop = func() { _ = srv.Close(); _ = os.Remove(sock) }
	case h.fsBin:
		var sock string
		for _, a := range ps.Args {
			if s, ok := strings.CutPrefix(a, "--socket-path="); ok {
				sock = s
			}
		}
		if !strings.Contains(strings.Join(ps.Args, " "), "--migration-mode=find-paths") {
			return nil, fmt.Errorf("fake virtiofsd: snapshots need --migration-mode=find-paths")
		}
		l, err := net.Listen("unix", sock)
		if err != nil {
			return nil, err
		}
		p.kind = "fs"
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		p.stop = func() { _ = l.Close(); _ = os.Remove(sock) }
	default:
		return nil, fmt.Errorf("fake host: no such binary %q", ps.Path)
	}
	h.mu.Lock()
	h.procs[p.pid] = p
	h.mu.Unlock()
	return p, nil
}

func (h *fakeHost) Find(pid int, marker string) Process {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.procs[pid]; ok && p.marker == marker {
		return p
	}
	gone := &fakeProc{h: h, pid: pid, done: make(chan struct{}), stop: func() {}}
	gone.exit()
	return gone
}

// fakeCH is one fake Cloud Hypervisor VMM: the v54 endpoints herd uses and their state rules.
type fakeCH struct {
	h    *fakeHost
	proc *fakeProc

	mu     sync.Mutex
	state  string // none, created, running, paused, shutdown
	config *vmConfig
}

func (c *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ep := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	c.h.record(ep)
	c.h.mu.Lock()
	hang, unreachable, delay, ignore := c.h.hang[ep], c.h.unreachable[ep], c.h.delay[ep], c.h.ignoreShutdown
	c.h.mu.Unlock()
	switch {
	case unreachable:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	case hang:
		select {
		case <-r.Context().Done():
		case <-c.proc.done:
		}
		return
	case delay > 0:
		time.Sleep(delay)
	}
	if want := map[string]string{epPing: http.MethodGet}[ep]; want == "" && r.Method != http.MethodPut || want != "" && r.Method != want {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func(code int, msg string) { http.Error(w, msg, code) }
	switch ep {
	case epPing:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pingResponse{BuildVersion: "v54.0", Version: "54.0", PID: int64(c.proc.pid)})
		return
	case epCreate:
		var cfg vmConfig
		if err := decodeStrict(r, &cfg); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if c.state != "none" {
			fail(http.StatusConflict, "VM already created")
			return
		}
		if err := checkConfig(&cfg); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		c.config, c.state = &cfg, "created"
	case epBoot:
		if c.state != "created" {
			fail(http.StatusNotFound, "VM not created")
			return
		}
		c.state = "running"
	case epPause:
		if c.state != "running" {
			fail(http.StatusMethodNotAllowed, "VM not running")
			return
		}
		c.state = "paused"
	case epResume:
		if c.state != "paused" {
			fail(http.StatusMethodNotAllowed, "VM not paused")
			return
		}
		c.state = "running"
	case epSnapshot:
		var sc snapshotConfig
		if err := decodeStrict(r, &sc); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if c.state != "paused" {
			fail(http.StatusMethodNotAllowed, "VM must be paused to snapshot")
			return
		}
		dir, ok := strings.CutPrefix(sc.DestinationURL, "file://")
		if !ok {
			fail(http.StatusBadRequest, "destination_url must be file://")
			return
		}
		cfg, _ := json.Marshal(c.config)
		for name, data := range map[string][]byte{"config.json": cfg, "state.json": []byte(`{}`), "memory-ranges": []byte("mem")} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				fail(http.StatusInternalServerError, err.Error())
				return
			}
		}
	case epRestore:
		var rc restoreConfig
		if err := decodeStrict(r, &rc); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if c.state != "none" {
			fail(http.StatusNotFound, "VM already created")
			return
		}
		dir, _ := strings.CutPrefix(rc.SourceURL, "file://")
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		var cfg vmConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if len(cfg.Fs) > 0 && rc.MemoryRestoreMode != "Copy" {
			fail(http.StatusBadRequest, "on-demand restore is not available with vhost-user devices")
			return
		}
		if err := checkConfig(&cfg); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		c.config, c.state = &cfg, "paused"
		if rc.Resume {
			c.state = "running"
		}
	case epShutdown:
		if ignore {
			break
		}
		if c.state != "running" && c.state != "paused" {
			fail(http.StatusMethodNotAllowed, "VM not started")
			return
		}
		c.state = "shutdown"
	case epVMMShutdown:
		if !ignore {
			defer func() { go func() { time.Sleep(time.Millisecond); c.proc.exit() }() }()
		}
	default:
		fail(http.StatusNotFound, "no such endpoint "+ep)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeStrict(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// checkConfig applies the rules real Cloud Hypervisor v54 enforces on what herd sends, and connects to the
// virtio-fs back-ends the way it does.
func checkConfig(c *vmConfig) error {
	switch {
	case c.Payload.Kernel == "":
		return fmt.Errorf("no kernel")
	case c.CPUs.BootVCPUs < 1 || c.Memory.Size <= 0:
		return fmt.Errorf("bad cpus/memory")
	case len(c.Fs) > 0 && !c.Memory.Shared:
		return fmt.Errorf("vhost-user devices need shared memory")
	case c.Vsock == nil || c.Vsock.CID < 3:
		return fmt.Errorf("vsock: bad cid")
	}
	for _, d := range c.Disks {
		if d.ImageType != "Raw" {
			return fmt.Errorf("disk %s: image_type %q", d.Path, d.ImageType)
		}
	}
	for _, f := range c.Fs {
		conn, err := net.Dial("unix", f.Socket)
		if err != nil {
			return fmt.Errorf("fs %s: %w", f.Tag, err)
		}
		_ = conn.Close()
	}
	return nil
}

// faultyVMM gives the contract its Faults by injecting them into the fake Cloud Hypervisor's endpoints.
type faultyVMM struct {
	*VMM
	h *fakeHost
}

var opEndpoints = map[vmm.Op][]string{
	vmm.OpBoot:     {epCreate, epBoot},
	vmm.OpSnapshot: {epPause, epSnapshot},
	vmm.OpRestore:  {epRestore},
	vmm.OpInspect:  {epPing},
	vmm.OpDelete:   {epShutdown, epVMMShutdown},
}

func (f *faultyVMM) Hang(op vmm.Op) {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	for _, ep := range opEndpoints[op] {
		f.h.hang[ep] = true
	}
}

func (f *faultyVMM) Unreachable(op vmm.Op) {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	for _, ep := range opEndpoints[op] {
		f.h.unreachable[ep] = true
	}
}

func (f *faultyVMM) LeaveOnDelete() {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	f.h.ignoreShutdown = true
}

func (f *faultyVMM) Clear() { f.h.clear() }
