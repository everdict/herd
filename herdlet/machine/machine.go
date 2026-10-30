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
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/everdict/herd/guest/initd"
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
	// Flush, if set, asks the guest to flush before every snapshot (GuestFlush); a flush that fails or does not
	// answer within FlushTimeout (default DefaultFlushTimeout) fails the snapshot.
	Flush        Flusher
	FlushTimeout time.Duration
	// Network, if set, gives each VM its link (Network): herd-guest's plan carries the guest's half, the spec the NIC.
	Network Network
}

// Machines boots and tracks one host's actor VMs.
type Machines struct {
	cfg Config

	mu sync.Mutex
	// ids maps an actor's VM id to the VMM's id for it, when they differ (an adopted warm VM, a MEMORY restore).
	ids map[string]string
	// tpl is the template each VMM id was booted from.
	tpl map[string]link.Template
	// links is what each VMM id's network lease gave it.
	links map[string]vmLink
}

// New returns Machines for cfg.
func New(cfg Config) (*Machines, error) {
	if cfg.VMM == nil || cfg.Images == nil || cfg.ShareDir == "" {
		return nil, errors.New("machine: VMM, Images and ShareDir are required")
	}
	if err := os.MkdirAll(cfg.ShareDir, 0o700); err != nil {
		return nil, fmt.Errorf("machine: %w", err)
	}
	return &Machines{cfg: cfg, ids: map[string]string{}, tpl: map[string]link.Template{}, links: map[string]vmLink{}}, nil
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
	if l, ok := m.link(vmID); ok {
		s.NIC = l.nic
	}
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

// errLinkTaken is answered by prepare when a MEMORY restore's link is another VM's on this host.
var errLinkTaken = errors.New("the VM's link is taken on this host")

// prepare builds the template's images, leases the VM its link (Config.Network; want is a MEMORY restore's) and lays
// out a fresh share for vmID with the plan and empty volumes. fill, if set, runs on the share before the volumes are
// laid (a restore unpacks its volume tree there). A failure other than errShareTaken and errLinkTaken leaves a lease
// the caller releases (unlease).
func (m *Machines) prepare(ctx context.Context, vmID string, t link.Template, fill func(*os.Root) error, want *initd.Network) (vmm.Spec, map[string]string, error) {
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
	if m.cfg.Network != nil {
		// Leased before the share is claimed: a VM id that holds a share holds its lease, which Lease answers again.
		g, nic, err := m.cfg.Network.Lease(ctx, vmID, want)
		if err != nil {
			if want != nil {
				return vmm.Spec{}, nil, fmt.Errorf("machine: link of %s: %w: %w", vmID, errLinkTaken, err)
			}
			return vmm.Spec{}, nil, fmt.Errorf("machine: link of %s: %w", vmID, err)
		}
		plan.Network = &g
		// herd-guest refuses a plan whose network it cannot configure; refuse it here, where it says why.
		if err := plan.Validate(); err != nil {
			return vmm.Spec{}, nil, fmt.Errorf("machine: link of %s: %w", vmID, err)
		}
		m.mu.Lock()
		m.links[vmID] = vmLink{guest: g, nic: nic}
		m.mu.Unlock()
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

func (m *Machines) link(vmID string) (vmLink, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[vmID]
	return l, ok
}

// up brings the host's side of the VM's link up before the VM starts.
func (m *Machines) up(ctx context.Context, vmID string) error {
	if m.cfg.Network == nil {
		return nil
	}
	if err := m.cfg.Network.Up(ctx, vmID); err != nil {
		return fmt.Errorf("machine: link of %s: %w", vmID, err)
	}
	return nil
}

// unlease releases the link of a VM that did not start, on a context of its own (the caller's may have ended).
func (m *Machines) unlease(ctx context.Context, vmID string) {
	if m.cfg.Network == nil {
		return
	}
	if p, err := m.cfg.Network.Release(context.WithoutCancel(ctx), vmID); p != vmm.PresenceGone {
		log.Printf("machine: releasing the link of %s, which did not start: %v: %v", vmID, p, err)
		return
	}
	m.mu.Lock()
	delete(m.links, vmID)
	m.mu.Unlock()
}

// Boot boots a VM of template t under id. A failed boot leaves no share and no link behind.
func (m *Machines) Boot(ctx context.Context, id string, t link.Template) error {
	spec, _, err := m.prepare(ctx, id, t, nil, nil)
	if err != nil {
		if !errors.Is(err, errShareTaken) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, id))
			m.unlease(ctx, id)
		}
		return err
	}
	err = m.up(ctx, id)
	if err == nil {
		_, err = m.cfg.VMM.Boot(ctx, spec)
	}
	if err != nil {
		err = m.withGuestBootError(id, err)
		if !errors.Is(err, vmm.ErrExists) {
			_ = os.RemoveAll(spec.Shares[len(spec.Shares)-1].HostPath)
			m.unlease(ctx, id)
		}
		return err
	}
	m.checkAttached(ctx, id)
	m.mu.Lock()
	m.tpl[id] = t
	m.mu.Unlock()
	return nil
}

// Snapshot has the guest flush what it buffered (Config.Flush), takes a snapshot of the actor's VM at f and writes
// its bundle to w. With stop (a suspend), the VM is stopped right after the VMM's snapshot and before the volume tree
// is read, so the tree is the one the actor left and the guest writes nothing more, and its link is taken down
// (verified; the lease stays for Restart); if writing the bundle then fails, Restart boots it again from its share.
// A MEMORY bundle records the VM's link, which its memory keeps.
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
	start := time.Now()
	if err := m.flush(ctx, vmID); err != nil {
		// Not %w: the guest's answer is no outcome of the VMM's, and no snapshot was taken.
		return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: flushing the guest: %v", id, err)
	}
	if m.cfg.Flush != nil {
		log.Printf("machine: snapshot %s: guest flushed after %v", id, time.Since(start).Round(time.Millisecond))
	}
	snap, err := m.cfg.VMM.Snapshot(ctx, vmID, f)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	if stop {
		if p, err := m.cfg.VMM.Delete(ctx, vmID); p != vmm.PresenceGone {
			return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: stopping the VM before reading its volumes: %s: %w", id, p, err)
		}
		if m.cfg.Network != nil {
			if p, err := m.cfg.Network.Down(ctx, vmID); p != vmm.PresenceGone {
				return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: taking the stopped VM's link down: %s: %w", id, p, err)
			}
		}
	}
	man := Manifest{Version: bundleVersion, Snapshot: snap, Template: t, Platform: caps.Platform, Arch: caps.Arch, Images: map[string]string{}}
	if l, ok := m.link(vmID); ok && snap.Fidelity == vmm.FidelityMemory {
		g := l.guest
		man.Network = &g
	}
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

// flush has the guest write back what it buffered, within the flush timeout.
func (m *Machines) flush(ctx context.Context, vmID string) error {
	if m.cfg.Flush == nil {
		return nil
	}
	d := m.cfg.FlushTimeout
	if d <= 0 {
		d = DefaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return m.cfg.Flush.Flush(ctx, vmID)
}

// Restart cold-boots a VM that Snapshot stopped, from its share as it is and on the link it still holds.
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
	if p, _ := m.cfg.VMM.Inspect(ctx, vmID); p == vmm.PresencePresent {
		return nil
	}
	if err := m.up(ctx, vmID); err != nil {
		return err
	}
	// The share is kept: a report of an earlier boot is not this one's.
	_ = os.Remove(filepath.Join(m.cfg.ShareDir, vmID, initd.BootErrorPath))
	if _, err := m.cfg.VMM.Boot(ctx, m.spec(vmID, t, disks)); err != nil && !errors.Is(err, vmm.ErrExists) {
		return m.withGuestBootError(vmID, err)
	}
	m.checkAttached(ctx, vmID)
	return nil
}

// ErrFamily is answered for a bundle this host cannot restore: a MEMORY snapshot of another restore family.
var ErrFamily = vmm.ErrRestoreFamilyMismatch

// Restore starts the actor id from a bundle: a VOLUMES bundle cold-boots the template here with its volume tree,
// whichever host or platform produced it (the images are built for this host's platform from the same references);
// a MEMORY bundle restores only in its own restore family, under the VM id it was taken from and on the link it was
// taken on. A MEMORY bundle of another family is refused before anything on this host is touched.
func (m *Machines) Restore(ctx context.Context, id string, r io.Reader) error {
	tr := tar.NewReader(r)
	man, err := readManifest(tr)
	if err != nil {
		return err
	}
	snap := man.Snapshot
	vmID, cold := id, false
	var want *initd.Network
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
		vmID, want = snap.VMID, man.Network
	default:
		return fmt.Errorf("machine: restore %s at %v: %w", id, snap.Fidelity, vmm.ErrFidelityUnsupported)
	}
	fill := func(root *os.Root) error {
		return extractVolumes(tr, root, man.Template.Volumes)
	}
	spec, _, err := m.prepare(ctx, vmID, man.Template, fill, want)
	if errors.Is(err, errShareTaken) && vmID != id || errors.Is(err, errLinkTaken) {
		// The VM id this memory restores under is taken on this host (another fork of one MEMORY tag, restored
		// before or at the same time, or the tag's source running again), or the link its memory keeps is another
		// VM's: one memory cannot run twice under one id or address, so this actor cold-boots from the bundle's
		// volume tree under its own id and on a link of its own, as a VOLUMES restore does. Creating the share is
		// the claim, so of two forks at once exactly one gets the memory; the bundle is still unread, since fill
		// runs only after the share was created.
		log.Printf("machine: restore %s: the memory of %s cannot run here (%v): cold boot from its volumes on a link of its own", id, vmID, err)
		vmID, cold = id, true
		spec, _, err = m.prepare(ctx, vmID, man.Template, fill, nil)
	}
	if err != nil {
		if !errors.Is(err, errShareTaken) && !errors.Is(err, errLinkTaken) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, vmID))
			m.unlease(ctx, vmID)
		}
		return err
	}
	err = m.up(ctx, vmID)
	if err == nil && snap.Fidelity == vmm.FidelityMemory && !cold {
		_, err = m.cfg.VMM.Restore(ctx, snap, spec)
	} else if err == nil {
		_, err = m.cfg.VMM.Boot(ctx, spec)
	}
	if err != nil {
		err = m.withGuestBootError(vmID, err)
		if !errors.Is(err, vmm.ErrExists) {
			_ = os.RemoveAll(filepath.Join(m.cfg.ShareDir, vmID))
			m.unlease(ctx, vmID)
		}
		return err
	}
	m.checkAttached(ctx, vmID)
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

// Delete deletes the actor's VM, its share and its link. Gone means all were verified gone: the VMM's verified
// delete, the share directory observed absent after its removal, and the link's verified release (Network.Release).
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
	if m.cfg.Network != nil {
		if p, err := m.cfg.Network.Release(ctx, vmID); p != vmm.PresenceGone {
			return p, fmt.Errorf("machine: the link of %s: %w", id, err)
		}
	}
	m.mu.Lock()
	delete(m.ids, id)
	delete(m.tpl, vmID)
	delete(m.links, vmID)
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
