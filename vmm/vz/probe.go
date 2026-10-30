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
	// Console is the console session the probe ran under (zero if the session did not answer).
	Console Console
}

// Passed reports whether this Mac may declare MEMORY.
func (r ProbeResult) Passed() bool { return r.Ran && r.Validated && r.RoundTrip && r.Reason == "" }

// Probe returns this Mac's save/restore probe result, running the probe when there is none that still stands (see
// probeFor). A probe that fails, times out or cannot run leaves the Mac at VOLUMES.
func (v *VMM) Probe(ctx context.Context) ProbeResult {
	con, err := v.console(ctx)
	return v.probeFor(ctx, con, err == nil)
}

// InvalidateProbe makes the next Capabilities run the probe again. herdlet calls it when the Mac wakes from sleep:
// the session, the keychain and the framework's state may all have changed while it slept.
func (v *VMM) InvalidateProbe() {
	v.probeMu.Lock()
	defer v.probeMu.Unlock()
	v.probeStale = true
}

// probeFor returns the probe result that stands for the console con (known only if conOK), running the probe again
// when there is none, it was invalidated, it is older than ProbeInterval, or it ran under a console that says
// nothing about this one: a probe from a locked screen, the login window or another user does not stand for an
// unlocked console.
func (v *VMM) probeFor(ctx context.Context, con Console, conOK bool) ProbeResult {
	v.probeMu.Lock()
	defer v.probeMu.Unlock()
	if r := v.probe; r != nil && !v.probeStale && time.Since(r.At) < v.cfg.ProbeInterval &&
		(!conOK || !con.Unlocked() || (r.Console.Unlocked() && r.Console.User == con.User)) {
		return *r
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.ProbeTimeout)
	defer cancel()
	r := v.runProbe(pctx)
	r.At = time.Now()
	if conOK {
		r.Console = con
	}
	v.probe, v.probeStale = &r, false
	return r
}

func (v *VMM) runProbe(ctx context.Context) ProbeResult {
	p := v.cfg.Probe
	if p.Kernel == "" {
		return ProbeResult{Reason: "no probe guest configured"}
	}
	r := ProbeResult{Ran: true}
	if err := os.MkdirAll(v.cfg.StateDir, 0o700); err != nil {
		r.Reason = err.Error()
		return r
	}
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
