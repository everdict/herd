package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/everdict/herd/vmm"
)

// ProbeResult is what this Mac showed about saving machine state. MEMORY is declared only when both
// validateSaveRestoreSupport answered true AND a real save/restore round-trip succeeded: the R0 spike saw
// validateSaveRestoreSupport = true and saveMachineStateTo refused ("permission denied") from SSH, launchctl asuser
// and a gui launchd agent.
type ProbeResult struct {
	// Ran is false when no probe guest was configured.
	Ran bool
	// Validated is validateSaveRestoreSupport's answer.
	Validated bool
	// RoundTrip is true when a VM was paused, saved, stopped, restored from the save file into a new machine and
	// resumed to running.
	RoundTrip bool
	// Reason says why MEMORY is not declared; empty when it is.
	Reason string
	// At is when the probe ran.
	At time.Time
}

// Passed reports whether this Mac may declare MEMORY.
func (r ProbeResult) Passed() bool { return r.Ran && r.Validated && r.RoundTrip && r.Reason == "" }

// Probe runs the save/restore probe once and returns its result; later calls return the same result. A probe that
// fails, times out or cannot run leaves the Mac at VOLUMES.
func (v *VMM) Probe(ctx context.Context) ProbeResult {
	v.probeMu.Lock()
	defer v.probeMu.Unlock()
	if v.probe != nil {
		return *v.probe
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.ProbeTimeout)
	defer cancel()
	r := v.runProbe(pctx)
	r.At = time.Now()
	v.probe = &r
	return r
}

func (v *VMM) runProbe(ctx context.Context) ProbeResult {
	p := v.cfg.Probe
	if p.Kernel == "" {
		return ProbeResult{Reason: "no probe guest configured"}
	}
	r := ProbeResult{Ran: true}
	dir, err := os.MkdirTemp(v.cfg.StateDir, ".probe-")
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	defer os.RemoveAll(dir)
	share := filepath.Join(dir, "share")
	if err := os.Mkdir(share, 0o700); err != nil {
		r.Reason = err.Error()
		return r
	}
	cmdline := p.Cmdline
	if cmdline == "" {
		cmdline = "console=hvc0"
	}
	// The same devices an actor has (virtio-fs share, vsock), since they decide whether state can be saved.
	mc := MachineConfig{Kernel: p.Kernel, Initrd: p.Initrd, Cmdline: cmdline, VCPUs: 1, MemoryMiB: 256,
		Shares: []vmm.Share{{Tag: "herd", HostPath: share}}, Console: filepath.Join(dir, "console.log")}

	first, err := callValue(ctx, "probe: new machine", func() (Machine, error) { return v.hv.NewMachine(mc) })
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	defer v.stopQuietly(ctx, first)
	ok, err := callValue(ctx, "probe: validateSaveRestoreSupport", first.ValidateSaveRestore)
	switch {
	case err != nil:
		r.Reason = err.Error()
		return r
	case !ok:
		r.Reason = "validateSaveRestoreSupport answered false"
		return r
	}
	r.Validated = true
	save := filepath.Join(dir, saveFile)
	steps := []struct {
		what string
		fn   func() error
	}{
		{"probe: start", first.Start},
		{"probe: pause", first.Pause},
		{"probe: saveMachineStateTo", func() error { return first.SaveState(save) }},
		{"probe: stop", first.Stop},
	}
	for _, s := range steps {
		if err := call(ctx, s.what, s.fn); err != nil {
			r.Reason = err.Error()
			return r
		}
	}
	if _, err := os.Stat(save); err != nil {
		r.Reason = fmt.Sprintf("probe: save reported success but left no file: %v", err)
		return r
	}
	mc.MachineID = first.MachineID()
	second, err := callValue(ctx, "probe: new machine", func() (Machine, error) { return v.hv.NewMachine(mc) })
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	defer v.stopQuietly(ctx, second)
	if err := call(ctx, "probe: restoreMachineStateFrom", func() error { return second.RestoreState(save) }); err != nil {
		r.Reason = err.Error()
		return r
	}
	if err := call(ctx, "probe: resume", second.Resume); err != nil {
		r.Reason = err.Error()
		return r
	}
	st, err := stateOf(ctx, second)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	if st != MachineRunning {
		r.Reason = fmt.Sprintf("probe: the restored VM is %s, not running", st)
		return r
	}
	r.RoundTrip = true
	return r
}

// stopQuietly stops a probe machine if it is still up; the probe's answer does not depend on it.
func (v *VMM) stopQuietly(ctx context.Context, m Machine) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.StopSettle)
	defer cancel()
	if st, err := stateOf(sctx, m); err == nil && !stopped(st) {
		_ = call(sctx, "probe: stop", m.Stop)
	}
}
