//go:build linux

package cloudhypervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/everdict/herd/vmm"
)

const backend = "cloud-hypervisor"

// guestCID is the guest's vsock CID. Hybrid vsock proxies through a per-VM unix socket, so CIDs never meet on the
// host and every VM can use the first guest CID.
const guestCID = 3

// maxSocketPath is the longest unix socket path the kernel accepts (sun_path holds 108 bytes with the NUL).
const maxSocketPath = 107

// Config locates the Cloud Hypervisor installation and the host's state.
type Config struct {
	// Binary is the cloud-hypervisor executable (pinned v54.0).
	Binary string
	// Virtiofsd is the virtiofsd executable (>= 1.11; snapshots need --migration-mode=find-paths).
	Virtiofsd string
	// VirtiofsdArgs are extra virtiofsd arguments, e.g. --sandbox=none when herdlet does not run as root.
	VirtiofsdArgs []string
	// StateDir holds one directory per VM: API, vsock and virtio-fs sockets, pid files and logs.
	StateDir string
	// SnapshotDir holds snapshots; hosts that share it can restore each other's snapshots (MEMORY ones only within
	// one restore family, at the same StateDir). Empty means StateDir/snapshots.
	SnapshotDir string
	// CPU is the host processor the restore family is derived from; nil means HostCPU().
	CPU CPU
	// Processes starts and finds the VMM and virtiofsd processes; nil means ExecProcesses.
	Processes Processes
	// ExitTimeout bounds how long Delete waits for the processes to exit before it answers present; zero means 10 s.
	ExitTimeout time.Duration
	// KernelLog, if set, answers the kernel's recent messages about pid (an OOM kill, a segfault, an audit record),
	// which a failed start logs with the VM's own logs; nil means KmsgLog, which reads /dev/kmsg.
	KernelLog func(pid int) []string
	// Cgroups, if set, gives each VM a cgroup of its own that its cloud-hypervisor and virtiofsd start in, limited to
	// the guest's memory and MemoryOverheadMiB (PLT-214); nil leaves them in the herdlet's own cgroup.
	Cgroups Cgroups
	// MemoryOverheadMiB is what a VM's cgroup may hold beyond its guest memory: the VMM, virtiofsd, page cache; zero
	// means DefaultMemoryOverheadMiB.
	MemoryOverheadMiB int
}

// VMM is the Cloud Hypervisor backend: one cloud-hypervisor process per VM, driven over its REST API, plus one
// virtiofsd per share.
type VMM struct {
	cfg       Config
	removeAll func(string) error

	mu  sync.Mutex
	vms map[string]*instance
}

var _ vmm.VMM = (*VMM)(nil)

// New returns the backend for cfg.
func New(cfg Config) *VMM {
	if cfg.SnapshotDir == "" && cfg.StateDir != "" {
		cfg.SnapshotDir = filepath.Join(cfg.StateDir, "snapshots")
	}
	if cfg.CPU == nil {
		cfg.CPU = HostCPU()
	}
	if cfg.Processes == nil {
		cfg.Processes = ExecProcesses{}
	}
	if cfg.ExitTimeout == 0 {
		cfg.ExitTimeout = 10 * time.Second
	}
	if cfg.KernelLog == nil {
		cfg.KernelLog = KmsgLog
	}
	if cfg.MemoryOverheadMiB == 0 {
		cfg.MemoryOverheadMiB = DefaultMemoryOverheadMiB
	}
	return &VMM{cfg: cfg, removeAll: os.RemoveAll, vms: map[string]*instance{}}
}

// instance is one VM's processes and files.
type instance struct {
	dir string
	// cgroup is the VM's cgroup directory its processes start in; empty without Config.Cgroups.
	cgroup string
	api    *client
	ch     Process
	fs     []Process
	// fsTag is the share each of fs serves, by index.
	fsTag []string
}

func (in *instance) procs() []Process {
	var ps []Process
	if in.ch != nil {
		ps = append(ps, in.ch)
	}
	return append(ps, in.fs...)
}

var (
	idRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	tagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,35}$`)
)

func (v *VMM) vmDir(id string) string { return filepath.Join(v.cfg.StateDir, "vms", id) }

func apiSocket(dir string) string            { return filepath.Join(dir, "api.sock") }
func fsSocket(dir string, tag string) string { return filepath.Join(dir, "fs-"+tag+".sock") }

func (v *VMM) Capabilities(ctx context.Context) (vmm.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Capabilities{}, fmt.Errorf("cloud-hypervisor: capabilities: %w", err)
	}
	class, err := v.cfg.CPU.Class()
	if err != nil {
		return vmm.Capabilities{}, err
	}
	family, err := vmm.CloudHypervisorRestoreFamily(v.cfg.CPU.Arch(), class)
	if err != nil {
		return vmm.Capabilities{}, err
	}
	return vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: v.cfg.CPU.Arch(),
		SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: family}, nil
}

func (v *VMM) validate(spec vmm.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	dir := v.vmDir(spec.ID)
	seen := map[string]bool{}
	for _, s := range spec.Shares {
		if !tagRE.MatchString(s.Tag) || seen[s.Tag] || s.HostPath == "" {
			return fmt.Errorf("%w: share %q at %q", vmm.ErrInvalidSpec, s.Tag, s.HostPath)
		}
		seen[s.Tag] = true
		if len(fsSocket(dir, s.Tag)) > maxSocketPath {
			return fmt.Errorf("%w: socket path %s is longer than %d bytes", vmm.ErrInvalidSpec, fsSocket(dir, s.Tag), maxSocketPath)
		}
	}
	if len(filepath.Join(dir, "vsock.sock")) > maxSocketPath {
		return fmt.Errorf("%w: state dir %s is too long for its sockets", vmm.ErrInvalidSpec, dir)
	}
	return nil
}

func (v *VMM) Boot(ctx context.Context, spec vmm.Spec) (vmm.VM, error) {
	if err := ctx.Err(); err != nil {
		return vmm.VM{}, fmt.Errorf("cloud-hypervisor: boot: %w", err)
	}
	if err := v.validate(spec); err != nil {
		return vmm.VM{}, err
	}
	return v.start(ctx, spec, func(ctx context.Context, in *instance) error {
		if err := in.api.put(ctx, epCreate, v.vmConfig(spec, in.dir)); err != nil {
			return err
		}
		return in.api.put(ctx, epBoot, nil)
	})
}

// start claims the VM's directory, launches virtiofsd and cloud-hypervisor and runs drive against the API. It
// answers only once drive has had its answers; on any failure it tears down what it launched.
func (v *VMM) start(ctx context.Context, spec vmm.Spec, drive func(context.Context, *instance) error) (vmm.VM, error) {
	dir := v.vmDir(spec.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return vmm.VM{}, fmt.Errorf("cloud-hypervisor: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return vmm.VM{}, fmt.Errorf("cloud-hypervisor: %s: %w", spec.ID, vmm.ErrExists)
		}
		return vmm.VM{}, fmt.Errorf("cloud-hypervisor: %w", err)
	}
	in := &instance{dir: dir, api: newClient(apiSocket(dir))}
	var err error
	if v.cfg.Cgroups != nil {
		in.cgroup, err = v.cfg.Cgroups.Create(spec.ID, int64(spec.MemoryMiB+v.cfg.MemoryOverheadMiB)<<20)
		if err != nil {
			err = fmt.Errorf("cloud-hypervisor: %s: %w", spec.ID, err)
		}
	}
	if err == nil {
		err = v.launch(ctx, spec, in)
	}
	if err == nil {
		err = drive(ctx, in)
	}
	if err != nil {
		// The VM's logs are read before its directory goes, and logged with how its processes ended.
		logs := readEvidence(in.dir)
		// Tear down on a context of its own: the caller's may be the one that just ran out.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*v.cfg.ExitTimeout)
		defer cancel()
		p, derr := v.teardown(tctx, spec.ID, in)
		if why := v.report(spec.ID, in, logs); why != "" {
			err = fmt.Errorf("%w; %s", err, why)
		}
		if p != vmm.PresenceGone {
			return vmm.VM{}, fmt.Errorf("%w (and the failed VM is %v: %v)", err, p, derr)
		}
		return vmm.VM{}, err
	}
	v.mu.Lock()
	v.vms[spec.ID] = in
	v.mu.Unlock()
	return vmm.VM{ID: spec.ID, State: vmm.StateRunning}, nil
}

// launch starts one virtiofsd per share, then cloud-hypervisor, and waits until each answers.
func (v *VMM) launch(ctx context.Context, spec vmm.Spec, in *instance) error {
	for _, s := range spec.Shares {
		sock := fsSocket(in.dir, s.Tag)
		args := VirtiofsdArgs(sock, s.HostPath, s.ReadOnly, v.cfg.VirtiofsdArgs)
		p, err := v.spawn(in, ProcSpec{Path: v.cfg.Virtiofsd, Args: args,
			Log: filepath.Join(in.dir, "virtiofsd-"+s.Tag+".log"), Marker: sock}, "virtiofsd-"+s.Tag)
		if err != nil {
			return err
		}
		in.fs = append(in.fs, p)
		in.fsTag = append(in.fsTag, s.Tag)
		if err := waitFor(ctx, "virtiofsd socket "+sock, p, func() (bool, error) {
			_, err := os.Stat(sock)
			return err == nil, nil
		}); err != nil {
			return err
		}
	}
	sock := apiSocket(in.dir)
	// -v: Cloud Hypervisor logs each step at info level to its stdout and stderr (the log), so a VMM that dies leaves
	// the last step it reached in the evidence of the failed start.
	p, err := v.spawn(in, ProcSpec{Path: v.cfg.Binary, Args: []string{"--api-socket", "path=" + sock, "-v"},
		Log: filepath.Join(in.dir, "cloud-hypervisor.log"), Marker: sock}, "cloud-hypervisor")
	if err != nil {
		return err
	}
	in.ch = p
	return waitFor(ctx, "cloud-hypervisor API "+sock, p, func() (bool, error) {
		_, err := in.api.ping(ctx)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, vmm.ErrUnreachable) {
			return false, nil // not listening yet
		}
		return false, err
	})
}

// spawn starts a process and records its pid so that a restarted herdlet can find it again.
func (v *VMM) spawn(in *instance, ps ProcSpec, name string) (Process, error) {
	ps.Cgroup = in.cgroup
	p, err := v.cfg.Processes.Start(ps)
	if err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: start %s: %w", name, err)
	}
	rec := strconv.Itoa(p.Pid()) + "\n" + ps.Marker + "\n"
	if err := os.WriteFile(filepath.Join(in.dir, name+".pid"), []byte(rec), 0o600); err != nil {
		_ = p.Signal(syscall.SIGKILL)
		return nil, fmt.Errorf("cloud-hypervisor: record %s: %w", name, err)
	}
	return p, nil
}

// waitFor polls ready until it holds, the process exits, or ctx ends.
func waitFor(ctx context.Context, what string, p Process, ready func() (bool, error)) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		ok, err := ready()
		if err != nil {
			return fmt.Errorf("cloud-hypervisor: wait for %s: %w", what, err)
		}
		if ok {
			return nil
		}
		if gone, err := p.Exited(); err != nil || gone {
			return fmt.Errorf("cloud-hypervisor: wait for %s: the process exited (%v)", what, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cloud-hypervisor: wait for %s: %w", what, ctx.Err())
		case <-t.C:
		}
	}
}

// DefaultCmdline is the guest kernel command line a spec without one boots with: the serial console Cloud
// Hypervisor writes to serial.log, so a failing guest is visible, and panic=-1, so a guest kernel panic ends the VM
// at once instead of hanging it.
func DefaultCmdline(arch string) string {
	if arch == "arm64" {
		return "console=ttyAMA0 panic=-1"
	}
	return "console=ttyS0 panic=-1"
}

func (v *VMM) vmConfig(spec vmm.Spec, dir string) vmConfig {
	cmdline := spec.Cmdline
	if cmdline == "" {
		cmdline = DefaultCmdline(v.cfg.CPU.Arch())
	}
	cmdline = vmm.WithFreePageReporting(cmdline)
	c := vmConfig{
		Payload: payloadConfig{Kernel: spec.Kernel, Initramfs: spec.Initrd, Cmdline: cmdline},
		CPUs:    cpusConfig{BootVCPUs: spec.VCPUs, MaxVCPUs: spec.VCPUs},
		Memory:  memoryConfig{Size: int64(spec.MemoryMiB) << 20, Shared: true},
		// A balloon that never inflates: it is there for the guest to report its free pages through, which Cloud
		// Hypervisor gives back to the host (vmm.FreePageReporting).
		Balloon: &balloonConfig{Size: 0, FreePageReporting: vmm.FreePageReporting},
		Vsock:   &vsockConfig{CID: guestCID, Socket: filepath.Join(dir, "vsock.sock")},
		Serial:  &consoleConfig{Mode: "File", File: filepath.Join(dir, "serial.log")},
		Console: &consoleConfig{Mode: "File", File: filepath.Join(dir, "console.log")},
	}
	for _, r := range spec.Rootfs {
		// Rootfs images are read-only and shared by every VM of the image (the image cache); the guest puts an overlay
		// on tmpfs on top (ADR A-10). Read-only also keeps Cloud Hypervisor from asking for a write lock on a file other VMs hold open.
		c.Disks = append(c.Disks, diskConfig{Path: r, ImageType: "Raw", Readonly: true})
	}
	for _, s := range spec.Shares {
		c.Fs = append(c.Fs, fsConfig{Tag: s.Tag, Socket: fsSocket(dir, s.Tag), NumQueues: 1, QueueSize: 1024})
	}
	if spec.NIC != nil {
		// The TAP and its egress table exist already (herdlet's, ADR A-18). A MEMORY restore re-opens the TAP the
		// snapshot's config names, so the restoring host re-creates the same one first.
		c.Net = []netConfig{{Tap: spec.NIC.TAP, MAC: spec.NIC.MAC}}
	}
	return c
}

// snapshotMeta is written last into a snapshot directory: a snapshot without it was never reported.
type snapshotMeta struct {
	ID            string       `json:"id"`
	VMID          string       `json:"vmId"`
	Fidelity      vmm.Fidelity `json:"fidelity"`
	RestoreFamily string       `json:"restoreFamily"`
}

const metaFile = "herd-snapshot.json"

func (v *VMM) Snapshot(ctx context.Context, id string, f vmm.Fidelity) (vmm.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("cloud-hypervisor: snapshot: %w", err)
	}
	var family string
	switch f {
	case vmm.FidelityVolumes:
		family = vmm.PortableRestoreFamily
	case vmm.FidelityMemory:
		c, err := v.Capabilities(ctx)
		if err != nil {
			return vmm.Snapshot{}, err
		}
		family = c.RestoreFamily
	default:
		return vmm.Snapshot{}, fmt.Errorf("cloud-hypervisor: snapshot %s at %v: %w", id, f, vmm.ErrFidelityUnsupported)
	}
	in, err := v.instance(id)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	if in == nil {
		return vmm.Snapshot{}, fmt.Errorf("cloud-hypervisor: snapshot %s: %w", id, vmm.ErrNotFound)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("cloud-hypervisor: snapshot id: %w", err)
	}
	snap := vmm.Snapshot{ID: id + "-" + hex.EncodeToString(rnd[:]), VMID: id, Fidelity: f, RestoreFamily: family}
	snap.Location = filepath.Join(v.cfg.SnapshotDir, snap.ID)
	tmp := filepath.Join(v.cfg.SnapshotDir, ".partial-"+snap.ID)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return vmm.Snapshot{}, fmt.Errorf("cloud-hypervisor: snapshot: %w", err)
	}
	if err := v.takeSnapshot(ctx, in, snap, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return vmm.Snapshot{}, err
	}
	return snap, nil
}

// takeSnapshot pauses the VM, has Cloud Hypervisor write a MEMORY snapshot into tmp, resumes the VM and publishes
// tmp under the snapshot's name only after every step answered. A VOLUMES snapshot pauses too, so the share is
// quiescent at the point it names; the volume tree itself stays in the share (DurableDir volumes are R2c's).
func (v *VMM) takeSnapshot(ctx context.Context, in *instance, snap vmm.Snapshot, tmp string) error {
	if err := in.api.put(ctx, epPause, nil); err != nil {
		v.resumeAfterFailure(ctx, in)
		return fmt.Errorf("cloud-hypervisor: snapshot %s: %w", snap.VMID, err)
	}
	if snap.Fidelity == vmm.FidelityMemory {
		if err := in.api.put(ctx, epSnapshot, snapshotConfig{DestinationURL: "file://" + tmp}); err != nil {
			v.resumeAfterFailure(ctx, in)
			return fmt.Errorf("cloud-hypervisor: snapshot %s: %w", snap.VMID, err)
		}
	}
	if err := in.api.put(ctx, epResume, nil); err != nil {
		return fmt.Errorf("cloud-hypervisor: snapshot %s: resume: %w", snap.VMID, err)
	}
	meta, err := json.Marshal(snapshotMeta{ID: snap.ID, VMID: snap.VMID, Fidelity: snap.Fidelity, RestoreFamily: snap.RestoreFamily})
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(tmp, metaFile), meta); err != nil {
		return fmt.Errorf("cloud-hypervisor: snapshot %s: %w", snap.VMID, err)
	}
	if err := os.Rename(tmp, snap.Location); err != nil {
		return fmt.Errorf("cloud-hypervisor: snapshot %s: publish: %w", snap.VMID, err)
	}
	return syncDir(v.cfg.SnapshotDir)
}

// resumeAfterFailure leaves the VM running when a snapshot failed after (or while) pausing it.
func (v *VMM) resumeAfterFailure(ctx context.Context, in *instance) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.ExitTimeout)
	defer cancel()
	_ = in.api.put(rctx, epResume, nil)
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (v *VMM) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	if err := ctx.Err(); err != nil {
		return vmm.VM{}, fmt.Errorf("cloud-hypervisor: restore: %w", err)
	}
	switch s.Fidelity {
	case vmm.FidelityMemory:
		// The family is decided here, before any process is started or Cloud Hypervisor is asked anything.
		c, err := v.Capabilities(ctx)
		if err != nil {
			return vmm.VM{}, err
		}
		if s.RestoreFamily != c.RestoreFamily {
			return vmm.VM{}, fmt.Errorf("cloud-hypervisor: restore %s: snapshot of %s on a %s host: %w", spec.ID, s.RestoreFamily, c.RestoreFamily, vmm.ErrRestoreFamilyMismatch)
		}
	case vmm.FidelityVolumes:
	default:
		return vmm.VM{}, fmt.Errorf("cloud-hypervisor: restore %s at %v: %w", spec.ID, s.Fidelity, vmm.ErrFidelityUnsupported)
	}
	if err := v.validate(spec); err != nil {
		return vmm.VM{}, err
	}
	dir, err := v.snapshotDir(s)
	if err != nil {
		return vmm.VM{}, err
	}
	if s.Fidelity == vmm.FidelityVolumes {
		return v.Boot(ctx, spec)
	}
	return v.start(ctx, spec, func(ctx context.Context, in *instance) error {
		// The snapshot's config names the paths of the host and state directory it was taken on: Cloud Hypervisor
		// restores from a copy of it that names this VM's.
		src, err := v.restoreSource(dir, spec, in.dir)
		if err != nil {
			return err
		}
		if err := in.api.put(ctx, epRestore, restoreConfig{SourceURL: "file://" + src, MemoryRestoreMode: "Copy"}); err != nil {
			return err
		}
		return in.api.put(ctx, epResume, nil)
	})
}

// snapshotDir finds the published snapshot s in the snapshot store and checks it is the one that was reported.
func (v *VMM) snapshotDir(s vmm.Snapshot) (string, error) {
	notFound := func(why string) error {
		return fmt.Errorf("cloud-hypervisor: snapshot %q: %s: %w", s.ID, why, vmm.ErrNotFound)
	}
	if !idRE.MatchString(s.ID) {
		return "", notFound("not a snapshot id")
	}
	dir := filepath.Join(v.cfg.SnapshotDir, s.ID)
	if err := checkMeta(dir, s); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", notFound("not in " + v.cfg.SnapshotDir)
		}
		return "", err
	}
	return dir, nil
}

// checkMeta checks that herd's record in dir names the reported snapshot s: fs.ErrNotExist without one, vmm.ErrNotFound
// for another snapshot's.
func checkMeta(dir string, s vmm.Snapshot) error {
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil {
		return fmt.Errorf("cloud-hypervisor: snapshot %q: %w", s.ID, err)
	}
	var m snapshotMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("cloud-hypervisor: snapshot %q: %w", s.ID, err)
	}
	if m != (snapshotMeta{ID: s.ID, VMID: s.VMID, Fidelity: s.Fidelity, RestoreFamily: s.RestoreFamily}) {
		return fmt.Errorf("cloud-hypervisor: snapshot %q: store holds %+v: %w", s.ID, m, vmm.ErrNotFound)
	}
	return nil
}

// instance returns the VM's processes, from memory or from its pid files; nil when the VM has no directory.
func (v *VMM) instance(id string) (*instance, error) {
	if !idRE.MatchString(id) {
		return nil, nil
	}
	v.mu.Lock()
	in := v.vms[id]
	v.mu.Unlock()
	if in != nil {
		return in, nil
	}
	dir := v.vmDir(id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: %s: %w", id, err)
	}
	in = &instance{dir: dir, api: newClient(apiSocket(dir))}
	pids, err := filepath.Glob(filepath.Join(dir, "*.pid"))
	if err != nil {
		return nil, err
	}
	for _, pf := range pids {
		data, err := os.ReadFile(pf)
		if err != nil {
			return nil, fmt.Errorf("cloud-hypervisor: %s: %w", id, err)
		}
		pidText, marker, _ := strings.Cut(string(data), "\n")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			return nil, fmt.Errorf("cloud-hypervisor: %s: bad pid file %s", id, pf)
		}
		p := v.cfg.Processes.Find(pid, strings.TrimSpace(marker))
		if filepath.Base(pf) == "cloud-hypervisor.pid" {
			in.ch = p
		} else {
			in.fs = append(in.fs, p)
			in.fsTag = append(in.fsTag, strings.TrimPrefix(strings.TrimSuffix(filepath.Base(pf), ".pid"), "virtiofsd-"))
		}
	}
	return in, nil
}

func (v *VMM) Inspect(ctx context.Context, id string) (vmm.Presence, error) {
	if err := ctx.Err(); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: inspect: %w", err)
	}
	in, err := v.instance(id)
	if err != nil {
		return vmm.PresenceUnknown, err
	}
	if in == nil {
		return vmm.PresenceGone, nil
	}
	if in.ch != nil {
		exited, err := in.ch.Exited()
		if err != nil {
			return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: inspect %s: %w", id, err)
		}
		if !exited {
			if _, err := in.api.ping(ctx); err != nil {
				return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: inspect %s: %w", id, err)
			}
		}
	}
	// The VMM answered, or it is gone but the VM's files remain: either way Delete has something to remove.
	return vmm.PresencePresent, nil
}

var _ vmm.ExitObserver = (*VMM)(nil)

// Exited answers whether the VM's cloud-hypervisor process has exited, or one of its virtiofsd processes has. The why
// is the process's exit status when this herdlet started it, and the last lines of its log. A VM whose virtiofsd
// exited is ended here (endForVirtiofsd).
func (v *VMM) Exited(ctx context.Context, id string) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", fmt.Errorf("cloud-hypervisor: exited: %w", err)
	}
	in, err := v.instance(id)
	if err != nil {
		return false, "", err
	}
	if in == nil {
		return true, "the VM is gone", nil
	}
	if in.ch == nil {
		return true, "no cloud-hypervisor process is recorded", nil
	}
	exited, err := in.ch.Exited()
	if err != nil {
		return false, "", fmt.Errorf("cloud-hypervisor: exited %s: %w", id, err)
	}
	if exited {
		return true, exitWhy("cloud-hypervisor", in.ch, filepath.Join(in.dir, "cloud-hypervisor.log")), nil
	}
	for i, fs := range in.fs {
		gone, err := fs.Exited()
		if err != nil {
			return false, "", fmt.Errorf("cloud-hypervisor: exited %s: %w", id, err)
		}
		if gone {
			return true, v.endForVirtiofsd(ctx, id, in, i), nil
		}
	}
	return false, "", nil
}

// endForVirtiofsd ends the VM whose virtiofsd in.fs[i] exited and answers why. Its share is gone for good: every
// access to it blocks (PLT-372), and after a guest restart, whose disconnect is what makes virtiofsd exit, Cloud
// Hypervisor v54 waits 60 s for a vhost-user connection that never comes, answering no API call (PLT-370). So the VM
// is one that exited, and its cloud-hypervisor is sent SIGKILL rather than left to its timeout. Only the herdlet's
// watch asks this, never while herd's own work on the VM (a delete, a suspend's stop) is in flight (herdlet.checkExit).
func (v *VMM) endForVirtiofsd(ctx context.Context, id string, in *instance, i int) string {
	why := exitWhy("virtiofsd of share "+strconv.Quote(in.fsTag[i]), in.fs[i], filepath.Join(in.dir, "virtiofsd-"+in.fsTag[i]+".log"))
	pid := in.ch.Pid()
	log.Printf("cloud-hypervisor: %s: %s: sending SIGKILL to cloud-hypervisor (pid %d)", id, why, pid)
	_ = in.ch.Signal(syscall.SIGKILL)
	if err := waitExited(ctx, []Process{in.ch}, v.cfg.ExitTimeout); err == nil {
		if gone, _ := in.ch.Exited(); gone {
			return fmt.Sprintf("%s; cloud-hypervisor (pid %d) was ended with SIGKILL", why, pid)
		}
	}
	return fmt.Sprintf("%s; cloud-hypervisor (pid %d) was sent SIGKILL and is not yet seen exited", why, pid)
}

// exitWhy is what is known of how process p, named name, ended: its exit status when this herdlet started it, and the
// last lines of its log.
func exitWhy(name string, p Process, logPath string) string {
	why := fmt.Sprintf("%s (pid %d) exited", name, p.Pid())
	if s, ok := p.(interface{ ExitStatus() string }); ok {
		if st := s.ExitStatus(); st != "" {
			why += " (" + st + ")"
		}
	}
	if tail := logTail(logPath, 3); tail != "" {
		why += "; its log ends: " + tail
	}
	return why
}

// logTail is the last n non-empty lines of a log, at most 1 KiB, joined by " | "; empty when there is none.
func logTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 1024
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-4*limit, 0)
	buf := make([]byte, fi.Size()-off)
	if n, _ := f.ReadAt(buf, off); n < len(buf) {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(buf), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s := strings.Join(lines, " | ")
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}

func (v *VMM) Delete(ctx context.Context, id string) (vmm.Presence, error) {
	if err := ctx.Err(); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete: %w", err)
	}
	in, err := v.instance(id)
	if err != nil {
		return vmm.PresenceUnknown, err
	}
	if in == nil {
		return vmm.PresenceGone, nil
	}
	// Logged: a delete is the one way herd itself ends a VM, so a VM that died under another step shows here if
	// it was asked to.
	log.Printf("cloud-hypervisor: delete %s requested", id)
	if in.ch != nil {
		if exited, err := in.ch.Exited(); err == nil && !exited {
			for _, ep := range []string{epShutdown, epVMMShutdown} {
				// A refusal (the VM was never booted) is an answer and does not stop the delete; no answer within
				// half the exit timeout while the caller's context still runs means a wedged VMM, which the signals
				// below deal with.
				sctx, cancel := context.WithTimeout(ctx, v.cfg.ExitTimeout/2)
				err := in.api.put(sctx, ep, nil)
				cancel()
				if err != nil && !isAPIError(err) && ctx.Err() != nil {
					return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete %s: %w (%v)", id, ctx.Err(), err)
				}
			}
		}
	}
	return v.teardown(ctx, id, in)
}

// teardown stops the VM's processes, observes them exited, removes the VM's directory and observes it absent. Only
// then is the answer gone.
func (v *VMM) teardown(ctx context.Context, id string, in *instance) (vmm.Presence, error) {
	procs := in.procs()
	half := v.cfg.ExitTimeout / 2
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		left, err := running(procs)
		if err != nil {
			return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete %s: %w", id, err)
		}
		if len(left) == 0 {
			break
		}
		for _, p := range left {
			log.Printf("cloud-hypervisor: %s: sending %v to pid %d", id, sig, p.Pid())
			_ = p.Signal(sig)
		}
		if err := waitExited(ctx, left, half); err != nil {
			return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete %s: %w", id, err)
		}
	}
	left, err := running(procs)
	if err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete %s: %w", id, err)
	}
	if len(left) > 0 {
		pids := make([]string, len(left))
		for i, p := range left {
			pids[i] = strconv.Itoa(p.Pid())
		}
		return vmm.PresencePresent, fmt.Errorf("cloud-hypervisor: delete %s: pid %s still running after SIGKILL: %w", id, strings.Join(pids, ","), vmm.ErrDeleteNotVerified)
	}
	if v.cfg.Cgroups != nil {
		// Its processes are gone, so is the cgroup they ran in; one left behind holds nothing and is taken again by
		// the next VM of this id.
		if err := v.cfg.Cgroups.Remove(id); err != nil {
			log.Printf("cloud-hypervisor: %s: %v", id, err)
		}
	}
	if err := v.removeAll(in.dir); err != nil {
		return vmm.PresencePresent, fmt.Errorf("cloud-hypervisor: delete %s: %v: %w", id, err, vmm.ErrDeleteNotVerified)
	}
	if _, err := os.Lstat(in.dir); err == nil {
		return vmm.PresencePresent, fmt.Errorf("cloud-hypervisor: delete %s: %s is still there: %w", id, in.dir, vmm.ErrDeleteNotVerified)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return vmm.PresenceUnknown, fmt.Errorf("cloud-hypervisor: delete %s: %w", id, err)
	}
	v.mu.Lock()
	if v.vms[id] == in {
		delete(v.vms, id)
	}
	v.mu.Unlock()
	return vmm.PresenceGone, nil
}

func running(procs []Process) ([]Process, error) {
	var left []Process
	for _, p := range procs {
		exited, err := p.Exited()
		if err != nil {
			return nil, err
		}
		if !exited {
			left = append(left, p)
		}
	}
	return left, nil
}

// waitExited waits up to d for procs to exit. It fails only when ctx ends; a process still running after d is the
// caller's to observe.
func waitExited(ctx context.Context, procs []Process, d time.Duration) error {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		left, err := running(procs)
		if err != nil || len(left) == 0 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-t.C:
		}
	}
}
