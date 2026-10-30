package vz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/everdict/herd/vmm"
)

// DefaultGuestPort is the vsock port herd-guest serves on (cmd/herd-guest serve -listen vsock://:8080).
const DefaultGuestPort = 8080

// Config describes the Mac.
type Config struct {
	// StateDir holds each VM's files (vms/<id>) and the probe's scratch files.
	StateDir string
	// SnapshotDir holds snapshot records and save files; empty means StateDir/snapshots. Hosts that restore each
	// other's VOLUMES snapshots read the same SnapshotDir.
	SnapshotDir string
	// Host labels this Mac; MEMORY snapshots restore only on it (microvm-vz-arm64-<host>).
	Host string
	// GuestPort is the vsock port Boot waits on before it answers; zero means DefaultGuestPort.
	GuestPort uint32
	// Probe is the guest the save/restore probe boots. Without a kernel there is no probe and the Mac declares
	// VOLUMES.
	Probe ProbeConfig
	// Network, if set, returns the host end of a VM's network: one end of a datagram socket pair whose other end
	// herd-egress's userspace stack holds. Nil boots VMs without a NIC.
	Network func(id string) (*os.File, error)
	// StopSettle bounds how long Delete waits, after Virtualization.framework accepted a stop, to observe the VM
	// stopped; zero means 2 s. A VM still running after it is present with vmm.ErrDeleteNotVerified.
	StopSettle time.Duration
	// ProbeTimeout bounds the save/restore probe; zero means 2 min.
	ProbeTimeout time.Duration
}

// ProbeConfig is the guest the save/restore probe boots: the actors' kernel and initramfs.
type ProbeConfig struct {
	Kernel  string
	Initrd  string
	Cmdline string
}

// VMM is the Virtualization.framework backend.
type VMM struct {
	cfg    Config
	hv     Hypervisor
	family string

	mu  sync.Mutex
	vms map[string]*instance
	seq int

	probeMu sync.Mutex
	probe   *ProbeResult
}

// instance is one VM this process started. busy serialises the operations that change it.
type instance struct {
	id   string
	dir  string
	busy chan struct{}

	// Set once started.
	m       Machine
	network *os.File
}

var _ vmm.VMM = (*VMM)(nil)

// New returns the backend for cfg on hv (Native() on a Mac, vzfake in tests).
func New(cfg Config, hv Hypervisor) (*VMM, error) {
	if hv == nil {
		return nil, fmt.Errorf("%w: vz: no hypervisor", vmm.ErrInvalidSpec)
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("%w: vz: no state directory", vmm.ErrInvalidSpec)
	}
	family, err := vmm.VZRestoreFamily(cfg.Host)
	if err != nil {
		return nil, err
	}
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = filepath.Join(cfg.StateDir, "snapshots")
	}
	if cfg.GuestPort == 0 {
		cfg.GuestPort = DefaultGuestPort
	}
	if cfg.StopSettle == 0 {
		cfg.StopSettle = 2 * time.Second
	}
	if cfg.ProbeTimeout == 0 {
		cfg.ProbeTimeout = 2 * time.Minute
	}
	return &VMM{cfg: cfg, hv: hv, family: family, vms: map[string]*instance{}}, nil
}

func (v *VMM) vmDir(id string) string { return filepath.Join(v.cfg.StateDir, "vms", id) }

// Capabilities declares VOLUMES unless this host's probe passed (see Probe).
func (v *VMM) Capabilities(ctx context.Context) (vmm.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Capabilities{}, fmt.Errorf("vz: capabilities: %w", err)
	}
	r := v.Probe(ctx)
	c := vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}
	if r.Passed() {
		c.SuspendMaxFidelity = vmm.FidelityMemory
		c.RestoreFamily = v.family
	}
	return c, nil
}

func (v *VMM) Boot(ctx context.Context, spec vmm.Spec) (vmm.VM, error) {
	if err := ctx.Err(); err != nil {
		return vmm.VM{}, fmt.Errorf("vz: boot: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return vmm.VM{}, err
	}
	inst, err := v.reserve(spec.ID)
	if err != nil {
		return vmm.VM{}, err
	}
	if err := v.start(ctx, inst, spec, machineConfig(spec), ""); err != nil {
		return vmm.VM{}, v.abandon(ctx, inst, err)
	}
	return vmm.VM{ID: spec.ID, State: vmm.StateRunning}, nil
}

func machineConfig(spec vmm.Spec) MachineConfig {
	mc := MachineConfig{Kernel: spec.Kernel, Initrd: spec.Initrd, Cmdline: spec.Cmdline, VCPUs: spec.VCPUs,
		MemoryMiB: spec.MemoryMiB, Shares: append([]vmm.Share(nil), spec.Shares...)}
	if mc.Cmdline == "" {
		mc.Cmdline = "console=hvc0"
	}
	for _, r := range spec.Rootfs {
		// Images are read-only; the guest puts an overlay on tmpfs on top (ADR A-10).
		mc.Disks = append(mc.Disks, Disk{Path: r, ReadOnly: true})
	}
	return mc
}

// reserve claims id for a VM about to start: ErrExists if this process runs it or its files are still on disk.
func (v *VMM) reserve(id string) (*instance, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.vms[id]; ok {
		return nil, fmt.Errorf("vz: %s: %w", id, vmm.ErrExists)
	}
	dir := v.vmDir(id)
	switch _, err := os.Lstat(dir); {
	case err == nil:
		return nil, fmt.Errorf("vz: %s: files of an earlier VM are still at %s: %w", id, dir, vmm.ErrExists)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("vz: %s: %w", id, err)
	}
	inst := &instance{id: id, dir: dir, busy: make(chan struct{}, 1)}
	v.vms[id] = inst
	return inst, nil
}

// start records spec in inst's directory, builds and starts the machine mc describes, and returns once the guest
// answers on its vsock port. With a save file it restores the machine's state instead of booting and resumes it.
func (v *VMM) start(ctx context.Context, inst *instance, spec vmm.Spec, mc MachineConfig, savePath string) error {
	if err := os.MkdirAll(inst.dir, 0o700); err != nil {
		return fmt.Errorf("vz: %s: %w", inst.id, err)
	}
	// A later snapshot rebuilds the machine from this record.
	if err := writeJSON(filepath.Join(inst.dir, specFile), spec); err != nil {
		return fmt.Errorf("vz: %s: %w", inst.id, err)
	}
	mc.Console = filepath.Join(inst.dir, "console.log")
	if v.cfg.Network != nil {
		f, err := v.cfg.Network(inst.id)
		if err != nil {
			return fmt.Errorf("vz: %s: network: %w", inst.id, err)
		}
		inst.network, mc.Network = f, f
	}
	m, err := callValue(ctx, "new machine", func() (Machine, error) { return v.hv.NewMachine(mc) })
	if err != nil {
		return err
	}
	v.mu.Lock()
	inst.m = m
	v.mu.Unlock()
	if savePath == "" {
		if err := call(ctx, "start "+inst.id, m.Start); err != nil {
			return err
		}
	} else {
		if err := call(ctx, "restore "+inst.id, func() error { return m.RestoreState(savePath) }); err != nil {
			return err
		}
		if err := call(ctx, "resume "+inst.id, m.Resume); err != nil {
			return err
		}
	}
	return v.waitGuest(ctx, inst.id, m)
}

// waitGuest returns once the guest accepts a vsock connection on the guest port: a started VM whose agent does not
// answer is not booted.
func (v *VMM) waitGuest(ctx context.Context, id string, m Machine) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := stateOf(ctx, m)
		if err != nil {
			return err
		}
		if st == MachineStopped || st == MachineError {
			return fmt.Errorf("vz: %s: the VM %s before its guest answered on vsock port %d", id, st, v.cfg.GuestPort)
		}
		// Virtualization.framework may never complete a connect to a port nobody listens on; bound each attempt.
		actx, cancel := context.WithTimeout(ctx, time.Second)
		err = call(actx, "connect "+id, func() error {
			c, err := m.ConnectVsock(v.cfg.GuestPort)
			if err != nil {
				return err
			}
			return c.Close()
		})
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vz: %s: guest did not answer on vsock port %d: %w (last: %v)", id, v.cfg.GuestPort, ctx.Err(), err)
		case <-tick.C:
		}
	}
}

// DialGuest opens a stream to a port the guest of VM id listens on, over its native virtio-vsock device. It is the
// host side of herd-guest's transport on a Mac (Cloud Hypervisor's is transport.DialHybridVsock).
func (v *VMM) DialGuest(ctx context.Context, id string, port uint32) (net.Conn, error) {
	inst := v.lookup(id)
	if inst == nil {
		return nil, fmt.Errorf("vz: dial %s: %w", id, vmm.ErrNotFound)
	}
	m := v.machine(inst)
	if m == nil {
		return nil, fmt.Errorf("vz: dial %s: still starting", id)
	}
	return callValue(ctx, fmt.Sprintf("connect %s:%d", id, port), func() (net.Conn, error) { return m.ConnectVsock(port) })
}

// abandon cleans up after a start that failed. The VM is forgotten only once it is verifiably gone; otherwise it
// stays known so that Inspect answers present (or unknown) rather than gone.
func (v *VMM) abandon(ctx context.Context, inst *instance, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.StopSettle+5*time.Second)
	defer cancel()
	if p, err := v.destroy(cctx, inst); p != vmm.PresenceGone {
		return fmt.Errorf("%w (and the VM is %v after cleanup: %v)", cause, p, err)
	}
	v.forget(inst)
	return cause
}

func (v *VMM) forget(inst *instance) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vms[inst.id] == inst {
		delete(v.vms, inst.id)
	}
}

func (v *VMM) lookup(id string) *instance {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.vms[id]
}

func (v *VMM) machine(inst *instance) Machine {
	v.mu.Lock()
	defer v.mu.Unlock()
	return inst.m
}

// acquire takes inst's operation slot within ctx.
func acquire(ctx context.Context, inst *instance) error {
	select {
	case inst.busy <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("vz: %s busy: %w", inst.id, ctx.Err())
	}
}

func release(inst *instance) { <-inst.busy }

func (v *VMM) Inspect(ctx context.Context, id string) (vmm.Presence, error) {
	if err := ctx.Err(); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("vz: inspect %s: %w", id, err)
	}
	if inst := v.lookup(id); inst != nil {
		m := v.machine(inst)
		if m == nil {
			return vmm.PresencePresent, nil // starting
		}
		// Any state the framework reports, stopped included, is a VM whose files are still here: present until a
		// Delete verifies otherwise. A state that does not come back is unknown.
		if _, err := stateOf(ctx, m); err != nil {
			return vmm.PresenceUnknown, err
		}
		return vmm.PresencePresent, nil
	}
	return filesPresence(v.vmDir(id))
}

// filesPresence answers from the VM's directory alone: a VM this process does not run is gone only if its files
// are absent.
func filesPresence(dir string) (vmm.Presence, error) {
	switch _, err := os.Lstat(dir); {
	case err == nil:
		return vmm.PresencePresent, nil
	case errors.Is(err, fs.ErrNotExist):
		return vmm.PresenceGone, nil
	default:
		return vmm.PresenceUnknown, fmt.Errorf("vz: %w", err)
	}
}

func (v *VMM) Delete(ctx context.Context, id string) (vmm.Presence, error) {
	if err := ctx.Err(); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("vz: delete %s: %w", id, err)
	}
	inst := v.lookup(id)
	if inst == nil {
		// Not running here (e.g. herdlet restarted, and the VM ended with the process): remove what is left.
		inst = &instance{id: id, dir: v.vmDir(id), busy: make(chan struct{}, 1)}
		return v.destroy(ctx, inst)
	}
	if err := acquire(ctx, inst); err != nil {
		return vmm.PresenceUnknown, err
	}
	defer release(inst)
	p, err := v.destroy(ctx, inst)
	if p == vmm.PresenceGone {
		v.forget(inst)
	}
	return p, err
}

// destroy stops inst's machine, observes it stopped, removes its files and observes them absent. Only then is the
// answer gone.
func (v *VMM) destroy(ctx context.Context, inst *instance) (vmm.Presence, error) {
	if m := v.machine(inst); m != nil {
		if p, err := v.stop(ctx, inst.id, m); p != vmm.PresenceGone {
			return p, err
		}
	}
	if inst.network != nil {
		_ = inst.network.Close()
	}
	if err := ctx.Err(); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("vz: delete %s: %w", inst.id, err)
	}
	rmErr := os.RemoveAll(inst.dir)
	switch p, err := filesPresence(inst.dir); p {
	case vmm.PresenceGone:
		return vmm.PresenceGone, nil
	case vmm.PresencePresent:
		return vmm.PresencePresent, fmt.Errorf("vz: delete %s: files remain at %s (%v): %w", inst.id, inst.dir, rmErr, vmm.ErrDeleteNotVerified)
	default:
		return vmm.PresenceUnknown, err
	}
}

// stop answers gone once the machine is observed stopped, present with ErrDeleteNotVerified if it is still running
// after the settle period, and unknown if the framework does not answer.
func (v *VMM) stop(ctx context.Context, id string, m Machine) (vmm.Presence, error) {
	st, err := stateOf(ctx, m)
	if err != nil {
		return vmm.PresenceUnknown, err
	}
	if stopped(st) {
		return vmm.PresenceGone, nil
	}
	stopErr := call(ctx, "stop "+id, m.Stop)
	if stopErr != nil && ctx.Err() != nil {
		return vmm.PresenceUnknown, stopErr
	}
	settle := time.NewTimer(v.cfg.StopSettle)
	defer settle.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := stateOf(ctx, m)
		if err != nil {
			return vmm.PresenceUnknown, err
		}
		if stopped(st) {
			return vmm.PresenceGone, nil
		}
		select {
		case <-ctx.Done():
			return vmm.PresenceUnknown, fmt.Errorf("vz: delete %s: still %s: %w", id, st, ctx.Err())
		case <-settle.C:
			return vmm.PresencePresent, fmt.Errorf("vz: delete %s: still %s after stop (stop: %v): %w", id, st, stopErr, vmm.ErrDeleteNotVerified)
		case <-tick.C:
		}
	}
}

func stopped(st MachineState) bool { return st == MachineStopped || st == MachineError }

// record is what a snapshot directory holds besides the save file: enough to restore without the VM.
type record struct {
	Snapshot vmm.Snapshot `json:"snapshot"`
	// Spec is the VM the snapshot was taken of; a MEMORY restore rebuilds exactly this machine.
	Spec vmm.Spec `json:"spec"`
	// MachineID is the generic platform identifier a save file is bound to (MEMORY only).
	MachineID []byte `json:"machineId,omitempty"`
}

const (
	recordFile = "snapshot.json"
	saveFile   = "state.vzvmsave"
)

// Snapshot pauses the VM, flushes its shares to stable storage, records the snapshot (and for MEMORY saves the
// machine state) and resumes the VM. A snapshot is returned only once its directory is complete and durable.
func (v *VMM) Snapshot(ctx context.Context, id string, f vmm.Fidelity) (vmm.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	switch f {
	case vmm.FidelityVolumes:
	case vmm.FidelityMemory:
		c, err := v.Capabilities(ctx)
		if err != nil {
			return vmm.Snapshot{}, err
		}
		if c.SuspendMaxFidelity < vmm.FidelityMemory {
			return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s at MEMORY: this Mac has not shown it can save machine state (%s): %w", id, v.Probe(ctx).Reason, vmm.ErrFidelityUnsupported)
		}
	default:
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s at %v: %w", id, f, vmm.ErrFidelityUnsupported)
	}
	inst := v.lookup(id)
	if inst == nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, vmm.ErrNotFound)
	}
	if err := acquire(ctx, inst); err != nil {
		return vmm.Snapshot{}, err
	}
	defer release(inst)
	m := v.machine(inst)
	if m == nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: still starting", id)
	}
	spec, err := readSpec(inst.dir)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	if st, err := stateOf(ctx, m); err != nil {
		return vmm.Snapshot{}, err
	} else if st != MachineRunning {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: the VM is %s, not running", id, st)
	}
	if err := call(ctx, "pause "+id, m.Pause); err != nil {
		_ = v.resume(ctx, id, m)
		return vmm.Snapshot{}, err
	}
	snap, err := v.record(ctx, id, f, spec, m)
	if rerr := v.resume(ctx, id, m); rerr != nil && err == nil {
		// The snapshot is complete but the VM was not left running; take the snapshot back so the caller's error
		// means "nothing happened".
		_ = os.RemoveAll(snap.Location)
		return vmm.Snapshot{}, rerr
	}
	if err != nil {
		return vmm.Snapshot{}, err
	}
	return snap, nil
}

// resume resumes a VM that is paused, with time of its own when the caller's context has run out: a snapshot
// that failed must not leave the actor paused.
func (v *VMM) resume(ctx context.Context, id string, m Machine) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.StopSettle+5*time.Second)
	defer cancel()
	st, err := stateOf(rctx, m)
	if err != nil {
		return err
	}
	if st != MachinePaused {
		return nil
	}
	return call(rctx, "resume "+id, m.Resume)
}

// record writes a snapshot of the paused VM into a scratch directory and renames it into place.
func (v *VMM) record(ctx context.Context, id string, f vmm.Fidelity, spec vmm.Spec, m Machine) (vmm.Snapshot, error) {
	v.mu.Lock()
	v.seq++
	snapID := v.cfg.Host + "-" + id + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(v.seq)
	v.mu.Unlock()
	if err := os.MkdirAll(v.cfg.SnapshotDir, 0o700); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	tmp, err := os.MkdirTemp(v.cfg.SnapshotDir, ".partial-"+snapID+"-")
	if err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	final := filepath.Join(v.cfg.SnapshotDir, snapID)
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	for _, s := range spec.Shares {
		if s.ReadOnly {
			continue
		}
		if err := syncTree(ctx, s.HostPath); err != nil {
			return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: flushing share %s: %w", id, s.Tag, err)
		}
	}
	rec := record{Snapshot: vmm.Snapshot{ID: snapID, VMID: id, Fidelity: f, RestoreFamily: vmm.PortableRestoreFamily, Location: final}, Spec: spec}
	if f == vmm.FidelityMemory {
		rec.Snapshot.RestoreFamily = v.family
		rec.MachineID = m.MachineID()
		if err := call(ctx, "save "+id, func() error { return m.SaveState(filepath.Join(tmp, saveFile)) }); err != nil {
			return vmm.Snapshot{}, err
		}
		if err := syncFile(filepath.Join(tmp, saveFile)); err != nil {
			return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
		}
	}
	if err := writeJSON(filepath.Join(tmp, recordFile), rec); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	if err := ctx.Err(); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	ok = true
	if err := syncFile(v.cfg.SnapshotDir); err != nil {
		_ = os.RemoveAll(final)
		return vmm.Snapshot{}, fmt.Errorf("vz: snapshot %s: %w", id, err)
	}
	return rec.Snapshot, nil
}

func (v *VMM) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	if err := ctx.Err(); err != nil {
		return vmm.VM{}, fmt.Errorf("vz: restore: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return vmm.VM{}, err
	}
	rec, err := v.readRecord(s)
	if err != nil {
		return vmm.VM{}, err
	}
	mc, recorded := machineConfig(spec), spec
	save := ""
	if s.Fidelity == vmm.FidelityMemory {
		c, err := v.Capabilities(ctx)
		if err != nil {
			return vmm.VM{}, err
		}
		if c.SuspendMaxFidelity < vmm.FidelityMemory || s.RestoreFamily != c.RestoreFamily {
			return vmm.VM{}, fmt.Errorf("vz: restore %s: snapshot of %s on a host of %q: %w", spec.ID, s.RestoreFamily, c.RestoreFamily, vmm.ErrRestoreFamilyMismatch)
		}
		// The save file restores only into the machine it was taken of.
		recorded = rec.Spec
		recorded.ID = spec.ID
		mc = machineConfig(recorded)
		mc.MachineID = rec.MachineID
		save = filepath.Join(v.cfg.SnapshotDir, s.ID, saveFile)
	} else if len(mc.Shares) == 0 {
		mc.Shares = append([]vmm.Share(nil), rec.Spec.Shares...)
		recorded.Shares = mc.Shares
	}
	inst, err := v.reserve(spec.ID)
	if err != nil {
		return vmm.VM{}, err
	}
	if err := v.start(ctx, inst, recorded, mc, save); err != nil {
		return vmm.VM{}, v.abandon(ctx, inst, err)
	}
	return vmm.VM{ID: spec.ID, State: vmm.StateRunning}, nil
}

// readRecord finds the snapshot s names and checks it is that snapshot.
func (v *VMM) readRecord(s vmm.Snapshot) (record, error) {
	if s.ID == "" || filepath.Base(s.ID) != s.ID || s.ID[0] == '.' {
		return record{}, fmt.Errorf("vz: snapshot %q: %w", s.ID, vmm.ErrNotFound)
	}
	b, err := os.ReadFile(filepath.Join(v.cfg.SnapshotDir, s.ID, recordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, fmt.Errorf("vz: snapshot %q: %w", s.ID, vmm.ErrNotFound)
	}
	if err != nil {
		return record{}, fmt.Errorf("vz: snapshot %q: %w", s.ID, err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return record{}, fmt.Errorf("vz: snapshot %q: %w", s.ID, err)
	}
	got := rec.Snapshot
	if got.ID != s.ID || got.VMID != s.VMID || got.Fidelity != s.Fidelity || got.RestoreFamily != s.RestoreFamily {
		return record{}, fmt.Errorf("vz: snapshot %q is not the one asked for (%+v): %w", s.ID, got, vmm.ErrNotFound)
	}
	return rec, nil
}

const specFile = "spec.json"

func readSpec(dir string) (vmm.Spec, error) {
	b, err := os.ReadFile(filepath.Join(dir, specFile))
	if err != nil {
		return vmm.Spec{}, fmt.Errorf("vz: %w", err)
	}
	var s vmm.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return vmm.Spec{}, fmt.Errorf("vz: %s: %w", dir, err)
	}
	return s, nil
}

// writeJSON writes v to path durably (temp file, fsync, rename).
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// syncTree flushes every regular file and directory under root to stable storage. The guest is paused, so what
// virtio-fs wrote through to the host is all there is; this makes it durable before the snapshot is recorded.
func syncTree(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		return syncFile(path)
	})
}
