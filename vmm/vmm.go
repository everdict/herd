// Package vmm is the boundary between herdlet and a hypervisor. Every herd actor is a Linux microVM (spec R-D7); this
// interface is what herdlet asks of Cloud Hypervisor on Ubuntu and of Virtualization.framework on macOS (R-D8), and
// what each host declares it can do (R-D9).
//
// Answers about whether a VM exists are three-valued (R-D6): gone, present or unknown. A call that could not find out
// answers unknown with the reason; it never answers gone.
package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Fidelity is what a snapshot holds. The numbers are Substrate's SnapshotFidelity numbers.
type Fidelity int32

const (
	FidelityUnspecified Fidelity = 0
	// FidelityVolumes holds the durable volumes only; restore cold-boots the containers.
	FidelityVolumes Fidelity = 1
	// FidelityMemory holds volumes, rootfs changes and guest memory; restore resumes where the actor left off.
	FidelityMemory Fidelity = 3
)

func (f Fidelity) String() string {
	switch f {
	case FidelityVolumes:
		return "VOLUMES"
	case FidelityMemory:
		return "MEMORY"
	case FidelityUnspecified:
		return "UNSPECIFIED"
	}
	return fmt.Sprintf("Fidelity(%d)", int32(f))
}

// Platform names a hypervisor backend.
type Platform string

const (
	PlatformCloudHypervisor Platform = "linux-cloud-hypervisor"
	PlatformVZ              Platform = "darwin-vz"
)

// PortableRestoreFamily is the family every VOLUMES snapshot belongs to: it restores on any host (R-D9).
const PortableRestoreFamily = "microvm-volumes"

var (
	archRE  = regexp.MustCompile(`^(amd64|arm64)$`)
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	tapRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	macRE   = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
)

// CloudHypervisorRestoreFamily names the family a Cloud Hypervisor MEMORY snapshot restores within:
// microvm-ch-<arch>-<cpu-class>. The CPU class must be a feature class the restoring host's CPUID is a superset of;
// the R0 spike saw restore refused across Intel generations ("incompatible CPUID entry").
func CloudHypervisorRestoreFamily(arch, cpuClass string) (string, error) {
	if !archRE.MatchString(arch) || !labelRE.MatchString(cpuClass) {
		return "", fmt.Errorf("%w: restore family needs arch amd64|arm64 and a lower-case cpu class, got %q %q", ErrInvalidSpec, arch, cpuClass)
	}
	return "microvm-ch-" + arch + "-" + cpuClass, nil
}

// VZRestoreFamily names the family a Virtualization.framework MEMORY snapshot restores within: one Mac (and user),
// microvm-vz-arm64-<host>, since the save file is sealed to them.
func VZRestoreFamily(host string) (string, error) {
	if !labelRE.MatchString(host) {
		return "", fmt.Errorf("%w: restore family needs a lower-case host label, got %q", ErrInvalidSpec, host)
	}
	return "microvm-vz-arm64-" + host, nil
}

// Capabilities is what a host declares, not what herd assumes (R-D9).
type Capabilities struct {
	Platform Platform
	// Arch is amd64 or arm64.
	Arch string
	// SuspendMaxFidelity is the highest fidelity Snapshot can take here: VOLUMES or MEMORY.
	SuspendMaxFidelity Fidelity
	// RestoreFamily is the family this host's MEMORY snapshots restore within; empty when it cannot take one.
	RestoreFamily string
	// Reason says why the host declares less than its platform could (e.g. a Mac whose console is locked); empty
	// otherwise.
	Reason string
}

// Share is a host directory exposed to the guest over virtio-fs.
type Share struct {
	Tag      string
	HostPath string
	ReadOnly bool
}

// Spec describes a VM to boot or to restore into.
type Spec struct {
	ID        string
	Kernel    string
	Initrd    string
	Cmdline   string
	Rootfs    []string
	VCPUs     int
	MemoryMiB int
	Shares    []Share
	// NIC, if set, is the VM's one network device on a host TAP (Cloud Hypervisor on Ubuntu, ADR A-18): the TAP must exist,
	// with its egress table loaded, before Boot or Restore. A backend whose NICs are not TAPs (vz) refuses it.
	NIC *NIC
}

// NIC is a VM's network device on a host TAP.
type NIC struct {
	// TAP is the host's TAP device (at most 15 characters).
	TAP string
	// MAC is the guest's address on it, xx:xx:xx:xx:xx:xx.
	MAC string
}

// Validate reports ErrInvalidSpec for a spec no backend could boot.
func (s Spec) Validate() error {
	switch {
	case !labelRE.MatchString(s.ID):
		return fmt.Errorf("%w: id %q", ErrInvalidSpec, s.ID)
	case s.Kernel == "":
		return fmt.Errorf("%w: no kernel", ErrInvalidSpec)
	case s.VCPUs < 1 || s.MemoryMiB < 1:
		return fmt.Errorf("%w: %d vCPU, %d MiB", ErrInvalidSpec, s.VCPUs, s.MemoryMiB)
	case s.NIC != nil && (!tapRE.MatchString(s.NIC.TAP) || !macRE.MatchString(s.NIC.MAC)):
		return fmt.Errorf("%w: nic tap %q mac %q", ErrInvalidSpec, s.NIC.TAP, s.NIC.MAC)
	}
	return nil
}

// State is a VM's run state.
type State int

const (
	StateUnknown State = iota
	StateRunning
	StatePaused
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	}
	return "unknown"
}

// VM is a booted or restored microVM.
type VM struct {
	ID    string
	State State
}

// Snapshot is a snapshot a VMM took and reported complete. A snapshot that was not reported is not durable and is
// never returned half-made.
type Snapshot struct {
	ID       string
	VMID     string
	Fidelity Fidelity
	// RestoreFamily is the producing host's family for MEMORY, PortableRestoreFamily for VOLUMES.
	RestoreFamily string
	// Location is backend-specific (a directory for Cloud Hypervisor, a save file for VZ).
	Location string
}

// Presence is the three-valued answer to "does this VM exist" (R-D6).
type Presence int

const (
	PresenceUnknown Presence = iota
	PresencePresent
	PresenceGone
)

func (p Presence) String() string {
	switch p {
	case PresencePresent:
		return "present"
	case PresenceGone:
		return "gone"
	}
	return "unknown"
}

// Op names a VMM operation.
type Op string

const (
	OpCapabilities Op = "capabilities"
	OpBoot         Op = "boot"
	OpSnapshot     Op = "snapshot"
	OpRestore      Op = "restore"
	OpInspect      Op = "inspect"
	OpDelete       Op = "delete"
)

// VMM is one host's hypervisor.
type VMM interface {
	// Capabilities reports what this host can do.
	Capabilities(ctx context.Context) (Capabilities, error)
	// Boot cold-boots a VM. ErrExists if the id is taken, ErrInvalidSpec for a bad spec.
	Boot(ctx context.Context, spec Spec) (VM, error)
	// Snapshot takes a snapshot of a running VM at the given fidelity and leaves the VM running. ErrNotFound for an
	// unknown VM, ErrFidelityUnsupported above SuspendMaxFidelity. On error no snapshot exists.
	Snapshot(ctx context.Context, id string, f Fidelity) (Snapshot, error)
	// Restore starts the VM spec.ID from a snapshot: MEMORY resumes, VOLUMES cold-boots with the volumes.
	// ErrRestoreFamilyMismatch for a MEMORY snapshot of another family, ErrNotFound for a snapshot that is not there,
	// ErrExists if spec.ID is running.
	Restore(ctx context.Context, s Snapshot, spec Spec) (VM, error)
	// Inspect answers whether the VM exists. PresenceUnknown always comes with the reason.
	Inspect(ctx context.Context, id string) (Presence, error)
	// Delete stops and removes the VM and verifies it is gone. It answers PresenceGone only when verified,
	// PresencePresent with ErrDeleteNotVerified when the VM survived, and PresenceUnknown with the reason otherwise.
	// Deleting a VM that does not exist is gone.
	Delete(ctx context.Context, id string) (Presence, error)
}

// ExitObserver is a VMM that can tell a VM it started that stopped running on its own (its hypervisor process exited:
// the guest powered off, the VMM crashed, something killed it; or a process the VM cannot run without, such as Cloud
// Hypervisor's virtiofsd, exited, and the backend ended the VM) from one that runs. herdlet watches the VMs it runs for
// actors with it, so herd-api learns of an exit nobody asked for.
type ExitObserver interface {
	// Exited answers true, with what is known of why, when the VM's hypervisor has exited or the VM is gone; false
	// while it runs; and an error (never true) when it could not be observed.
	Exited(ctx context.Context, id string) (bool, string, error)
}

var (
	ErrNotFound              = errors.New("vmm: not found")
	ErrExists                = errors.New("vmm: already exists")
	ErrInvalidSpec           = errors.New("vmm: invalid spec")
	ErrFidelityUnsupported   = errors.New("vmm: fidelity not supported on this host")
	ErrRestoreFamilyMismatch = errors.New("vmm: snapshot belongs to another restore family")
	ErrUnreachable           = errors.New("vmm: hypervisor did not answer")
	ErrDeleteNotVerified     = errors.New("vmm: delete not verified: the VM is still there")
	ErrNotImplemented        = errors.New("vmm: not implemented")
)

// NotImplementedError is what a backend that exists only as a stub answers.
type NotImplementedError struct {
	Backend string
	Op      Op
}

func (e *NotImplementedError) Error() string {
	return fmt.Sprintf("vmm: %s: %s not implemented", e.Backend, e.Op)
}

// Is makes errors.Is(err, ErrNotImplemented) true.
func (e *NotImplementedError) Is(target error) bool { return target == ErrNotImplemented }

// SnapshotCarrier is a VMM whose snapshots can be carried to another host: herdlet puts the files of a MEMORY
// snapshot it took into the snapshot's bundle, so the snapshot store holds everything a restore needs, and gives them
// back on the host that restores it (ADR A-28). A VMM that is not one keeps its MEMORY snapshots on the host that
// took them.
type SnapshotCarrier interface {
	// ExportSnapshot lists the files of snapshot s on this host. ErrNotFound if s is not here.
	ExportSnapshot(ctx context.Context, s Snapshot) ([]SnapshotFile, error)
	// ImportSnapshot keeps the files next yields (io.EOF after the last) as snapshot s on this host, so Restore(s)
	// finds it. A snapshot s already here is kept and the files are not read. On error nothing is kept.
	ImportSnapshot(ctx context.Context, s Snapshot, next func() (SnapshotFile, io.Reader, error)) error
}

// SnapshotFile is one file of a snapshot.
type SnapshotFile struct {
	// Name is a plain file name, without a directory.
	Name string
	Size int64
	// Open reads the file (ExportSnapshot only).
	Open func() (io.ReadCloser, error) `json:"-"`
}

// SnapshotRemover is a VMM that keeps the snapshots it takes and imports on its host until herdlet removes them
// (PLT-374). herdlet removes only snapshots of a VMM that is also a SnapshotCarrier: the bundle in the snapshot store
// then holds every file a restore needs, so the host's copy is never the only one.
type SnapshotRemover interface {
	// LocalSnapshots lists the ids of the snapshots published on this host.
	LocalSnapshots(ctx context.Context) ([]string, error)
	// RemoveSnapshot removes the snapshot id from this host and verifies it is gone; one that is not here is gone.
	RemoveSnapshot(ctx context.Context, id string) error
	// RemoveLeftovers removes what a Snapshot or ImportSnapshot that never finished left on this host (never
	// published, so nothing restores from it). It is for a start, while neither runs.
	RemoveLeftovers(ctx context.Context) error
}
