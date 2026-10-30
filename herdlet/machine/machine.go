// Package machine is the part of herdlet that turns an actor template into a microVM (spec R2, ADR A-19): it builds
// each container's image into a read-only rootfs (package image), lays the template's DurableDir volumes out as
// directories of the VM's own virtio-fs share and writes herd-guest's plan there, boots the VM through vmm.VMM, keeps
// warm VMs per template (Pool), and packs and unpacks the snapshot bundle a VOLUMES move carries between hosts.
//
// VM ids: an actor's VM is known to herd-api by the actor's id. A warm VM is booted under an id of its own and
// adopted by the actor that takes it; Machines maps the one to the other.
package machine

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// Images builds rootfs images (an *image.Cache).
type Images interface {
	Get(ctx context.Context, ref string) (image.Rootfs, error)
}

// GuestDialer reaches a port in a VM's guest (herdlet.GuestDialer).
type GuestDialer interface {
	DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error)
}

// Config is what Machines needs from its host.
type Config struct {
	VMM    vmm.VMM
	Images Images
	// ShareDir holds one share directory per VM, ShareDir/<vm id>. It must not be shared with anything else.
	ShareDir string
	// Base is what every VM boots with (kernel, initrd, command line); ID, Rootfs and Shares are set per VM, and
	// VCPUs and MemoryMiB when the template sets them.
	Base vmm.Spec
	// Guests, if set, is how DialGuest reaches a guest.
	Guests GuestDialer
}

// Machines boots and tracks one host's actor VMs.
type Machines struct {
	cfg Config

	mu sync.Mutex
	// ids maps an actor's VM id to the VMM's id for it, when they differ (an adopted warm VM, a MEMORY restore).
	ids map[string]string
	// tpl is the template each VMM id was booted from.
	tpl map[string]link.Template
}

// New returns Machines for cfg.
func New(cfg Config) (*Machines, error) {
	if cfg.VMM == nil || cfg.Images == nil || cfg.ShareDir == "" {
		return nil, errors.New("machine: VMM, Images and ShareDir are required")
	}
	if err := os.MkdirAll(cfg.ShareDir, 0o700); err != nil {
		return nil, fmt.Errorf("machine: %w", err)
	}
	return &Machines{cfg: cfg, ids: map[string]string{}, tpl: map[string]link.Template{}}, nil
}

// VMM is the host's hypervisor.
func (m *Machines) VMM() vmm.VMM { return m.cfg.VMM }

// ShareDir is the host directory of the VM's share.
func (m *Machines) ShareDir(id string) string { return filepath.Join(m.cfg.ShareDir, m.resolve(id)) }

func (m *Machines) resolve(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.ids[id]; ok {
		return v
	}
	return id
}

// Adopt makes the VMM's VM vmID the actor's VM id.
func (m *Machines) Adopt(id, vmID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != vmID {
		m.ids[id] = vmID
	}
}

func (m *Machines) spec(vmID string, t link.Template, disks []string) vmm.Spec {
	s := m.cfg.Base
	s.ID, s.Rootfs = vmID, disks
	s.Shares = append(append([]vmm.Share(nil), m.cfg.Base.Shares...), vmm.Share{Tag: ShareTag, HostPath: filepath.Join(m.cfg.ShareDir, vmID)})
	if t.VCPUs > 0 {
		s.VCPUs = t.VCPUs
	}
	if t.MemoryMiB > 0 {
		s.MemoryMiB = t.MemoryMiB
	}
	if s.VCPUs == 0 {
		s.VCPUs = 1
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = 512
	}
	return s
}

// errShareTaken is answered by prepare when the VM id already has a share on this host: another VM holds it.
var errShareTaken = errors.New("the VM id is taken on this host")

// prepare builds the template's images and lays out a fresh share for vmID with the plan and empty volumes. fill,
// if set, runs on the share before the volumes are laid (a restore unpacks its volume tree there).
func (m *Machines) prepare(ctx context.Context, vmID string, t link.Template, fill func(*os.Root) error) (vmm.Spec, map[string]string, error) {
	if len(t.Containers) == 0 {
		return vmm.Spec{}, nil, fmt.Errorf("machine: template %q has no container", t.UID)
	}
	roots := map[string]image.Rootfs{}
	digests := map[string]string{}
	for _, c := range t.Containers {
		if _, ok := roots[c.Image]; ok {
			continue
		}
		r, err := m.cfg.Images.Get(ctx, c.Image)
		if err != nil {
			return vmm.Spec{}, nil, fmt.Errorf("machine: container %q: %w", c.Name, err)
		}
		roots[c.Image], digests[c.Image] = r, r.Digest
	}
	plan, disks, err := Plan(t, roots)
	if err != nil {
		return vmm.Spec{}, nil, err
	}
	share := filepath.Join(m.cfg.ShareDir, vmID)
	if err := os.Mkdir(share, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return vmm.Spec{}, nil, fmt.Errorf("machine: share of %s: %w: %w", vmID, errShareTaken, err)
		}
		return vmm.Spec{}, nil, fmt.Errorf("machine: share of %s: %w", vmID, err)
	}
	root, err := os.OpenRoot(share)
	if err != nil {
		return vmm.Spec{}, nil, fmt.Errorf("machine: %w", err)
	}
	defer root.Close()
	if fill != nil {
		if err := fill(root); err != nil {
			return vmm.Spec{}, nil, err
		}
	}
	if err := layVolumes(root, plan); err != nil {
		return vmm.Spec{}, nil, err
	}
	if err := writePlan(root, plan); err != nil {
		return vmm.Spec{}, nil, err
	}
	return m.spec(vmID, t, disks), digests, nil
}

// Boot boots a VM of template t under id. A failed boot leaves no share behind.
func (m *Machines) Boot(ctx context.Context, id string, t link.Template) error {
	spec, _, err := m.prepare(ctx, id, t, nil)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, id))
		}
		return err
	}
	if _, err := m.cfg.VMM.Boot(ctx, spec); err != nil {
		if !errors.Is(err, vmm.ErrExists) {
			_ = os.RemoveAll(spec.Shares[len(spec.Shares)-1].HostPath)
		}
		return err
	}
	m.mu.Lock()
	m.tpl[id] = t
	m.mu.Unlock()
	return nil
}

// Snapshot takes a snapshot of the actor's VM at f and writes its bundle to w. With stop (a suspend), the VM is
// stopped right after the VMM's snapshot and before the volume tree is read, so the tree is the one the actor left
// and the guest writes nothing more; if writing the bundle then fails, Restart boots it again from its share.
func (m *Machines) Snapshot(ctx context.Context, id string, f vmm.Fidelity, stop bool, w io.Writer) (vmm.Snapshot, error) {
	vmID := m.resolve(id)
	m.mu.Lock()
	t, ok := m.tpl[vmID]
	m.mu.Unlock()
	if !ok {
		return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: %w", id, vmm.ErrNotFound)
	}
	caps, err := m.cfg.VMM.Capabilities(ctx)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	snap, err := m.cfg.VMM.Snapshot(ctx, vmID, f)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	if stop {
		if p, err := m.cfg.VMM.Delete(ctx, vmID); p != vmm.PresenceGone {
			return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: stopping the VM before reading its volumes: %s: %w", id, p, err)
		}
	}
	man := Manifest{Version: bundleVersion, Snapshot: snap, Template: t, Platform: caps.Platform, Arch: caps.Arch, Images: map[string]string{}}
	for _, c := range t.Containers {
		if r, err := m.cfg.Images.Get(ctx, c.Image); err == nil {
			man.Images[c.Image] = r.Digest
		}
	}
	if err := writeBundle(w, man, filepath.Join(m.cfg.ShareDir, vmID)); err != nil {
		return vmm.Snapshot{}, err
	}
	return snap, nil
}

// Restart cold-boots a VM that Snapshot stopped, from its share as it is.
func (m *Machines) Restart(ctx context.Context, id string) error {
	vmID := m.resolve(id)
	m.mu.Lock()
	t, ok := m.tpl[vmID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("machine: restart %s: %w", id, vmm.ErrNotFound)
	}
	roots := map[string]image.Rootfs{}
	for _, c := range t.Containers {
		r, err := m.cfg.Images.Get(ctx, c.Image)
		if err != nil {
			return err
		}
		roots[c.Image] = r
	}
	_, disks, err := Plan(t, roots)
	if err != nil {
		return err
	}
	if _, err := m.cfg.VMM.Boot(ctx, m.spec(vmID, t, disks)); err != nil && !errors.Is(err, vmm.ErrExists) {
		return err
	}
	return nil
}

// ErrFamily is answered for a bundle this host cannot restore: a MEMORY snapshot of another restore family.
var ErrFamily = vmm.ErrRestoreFamilyMismatch

// Restore starts the actor id from a bundle: a VOLUMES bundle cold-boots the template here with its volume tree,
// whichever host or platform produced it (the images are built for this host's platform from the same references);
// a MEMORY bundle restores only in its own restore family, under the VM id it was taken from. A MEMORY bundle of
// another family is refused before anything on this host is touched.
func (m *Machines) Restore(ctx context.Context, id string, r io.Reader) error {
	tr := tar.NewReader(r)
	man, err := readManifest(tr)
	if err != nil {
		return err
	}
	snap := man.Snapshot
	vmID, cold := id, false
	switch snap.Fidelity {
	case vmm.FidelityVolumes:
	case vmm.FidelityMemory:
		caps, err := m.cfg.VMM.Capabilities(ctx)
		if err != nil {
			return err
		}
		if caps.SuspendMaxFidelity < vmm.FidelityMemory || caps.RestoreFamily != snap.RestoreFamily {
			return fmt.Errorf("machine: restore %s: a MEMORY snapshot of %q on a host of %q: %w", id, snap.RestoreFamily, caps.RestoreFamily, ErrFamily)
		}
		vmID = snap.VMID
	default:
		return fmt.Errorf("machine: restore %s at %v: %w", id, snap.Fidelity, vmm.ErrFidelityUnsupported)
	}
	fill := func(root *os.Root) error {
		return extractVolumes(tr, root, man.Template.Volumes)
	}
	spec, _, err := m.prepare(ctx, vmID, man.Template, fill)
	if errors.Is(err, errShareTaken) && vmID != id {
		// The VM id this memory restores under is taken on this host (another fork of one MEMORY tag, restored
		// before or at the same time, or the tag's source running again): one memory cannot run twice under one
		// id, so this actor cold-boots from the bundle's volume tree under its own id, as a VOLUMES restore does.
		// Creating the share is the claim, so of two forks at once exactly one gets the memory; the bundle is
		// still unread, since fill runs only after the share was created.
		vmID, cold = id, true
		spec, _, err = m.prepare(ctx, vmID, man.Template, fill)
	}
	if err != nil {
		if !errors.Is(err, errShareTaken) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, vmID))
		}
		return err
	}
	if snap.Fidelity == vmm.FidelityMemory && !cold {
		_, err = m.cfg.VMM.Restore(ctx, snap, spec)
	} else {
		_, err = m.cfg.VMM.Boot(ctx, spec)
	}
	if err != nil {
		if !errors.Is(err, vmm.ErrExists) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, vmID))
		}
		return err
	}
	m.mu.Lock()
	m.tpl[vmID] = man.Template
	m.mu.Unlock()
	m.Adopt(id, vmID)
	return nil
}

// Inspect answers whether the actor's VM exists.
func (m *Machines) Inspect(ctx context.Context, id string) (vmm.Presence, error) {
	return m.cfg.VMM.Inspect(ctx, m.resolve(id))
}

// Delete deletes the actor's VM and its share. Gone means both were verified gone: the VMM's verified delete, and the
// share directory observed absent after its removal.
func (m *Machines) Delete(ctx context.Context, id string) (vmm.Presence, error) {
	vmID := m.resolve(id)
	p, err := m.cfg.VMM.Delete(ctx, vmID)
	if p != vmm.PresenceGone {
		return p, err
	}
	share := filepath.Join(m.cfg.ShareDir, vmID)
	if err := os.RemoveAll(share); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("machine: removing the share of %s: %w", id, err)
	}
	if _, err := os.Lstat(share); !errors.Is(err, fs.ErrNotExist) {
		return vmm.PresencePresent, fmt.Errorf("machine: the share of %s is still there (%v): %w", id, err, vmm.ErrDeleteNotVerified)
	}
	m.mu.Lock()
	delete(m.ids, id)
	delete(m.tpl, vmID)
	m.mu.Unlock()
	return vmm.PresenceGone, nil
}

// DialGuest reaches a port of the actor's guest.
func (m *Machines) DialGuest(ctx context.Context, id string, port uint32) (net.Conn, error) {
	if m.cfg.Guests == nil {
		return nil, fmt.Errorf("machine: no guest dialer")
	}
	return m.cfg.Guests.DialGuest(ctx, m.resolve(id), port)
}

// VMID is the VMM's id of the actor's VM (a warm VM it adopted keeps its own).
func (m *Machines) VMID(id string) string { return m.resolve(id) }
