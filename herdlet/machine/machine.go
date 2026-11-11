// Package machine is the part of herdlet that turns an actor template into a microVM (spec R2, ADR A-19): it builds
// each container's image into a read-only rootfs (package image), lays the template's DurableDir volumes out as
// directories of the VM's own virtio-fs share and writes herd-guest's plan there, boots the VM through vmm.VMM, keeps
// warm VMs per template (Pool), and packs and unpacks the snapshot bundle a move carries between hosts (with a
// MEMORY snapshot's hypervisor files, when the VMM carries them).
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
	"maps"
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
	// answer within FlushTimeout (default FlushTimeoutFor the VM's memory) fails the snapshot.
	Flush        Flusher
	FlushTimeout time.Duration
	// Clock, if set, hands the guest the host's wall clock (GuestClock) whenever it runs on from a snapshot's memory: a
	// MEMORY restore (refused when the clock cannot be set, see Restore), a VM a snapshot paused, and a warm VM taken.
	// ClockTimeout (default DefaultClockTimeout) bounds it.
	Clock        Clocker
	ClockTimeout time.Duration
	// Network, if set, gives each VM its link (Network): herd-guest's plan carries the guest's half, the spec the NIC.
	Network Network
}

// Machines boots and tracks one host's actor VMs.
type Machines struct {
	cfg Config

	mu sync.Mutex
	// ids maps an actor's VM id to the VMM's id for it, when they differ (an adopted warm VM).
	ids map[string]string
	// tpl is the template each VMM id was booted from.
	tpl map[string]link.Template
	// links is what each VMM id's network lease gave it.
	links map[string]vmLink
	// images is the image each VMM id runs per reference: what its snapshots pin.
	images map[string]map[string]image.Rootfs
	// pins is the digest each template's references resolved to on their first boot here (template uid →
	// reference → digest); pinMu serializes the first resolve of one.
	pins  map[string]map[string]string
	pinMu map[string]*sync.Mutex
	// snaps is the VMM snapshots this host keeps: in use by a restore, being removed, taken for an actor (snaps.go).
	snaps snapUse
}

// New returns Machines for cfg.
func New(cfg Config) (*Machines, error) {
	if cfg.VMM == nil || cfg.Images == nil || cfg.ShareDir == "" {
		return nil, errors.New("machine: VMM, Images and ShareDir are required")
	}
	if err := os.MkdirAll(cfg.ShareDir, 0o700); err != nil {
		return nil, fmt.Errorf("machine: %w", err)
	}
	return &Machines{cfg: cfg, ids: map[string]string{}, tpl: map[string]link.Template{}, links: map[string]vmLink{},
		images: map[string]map[string]image.Rootfs{}, pins: map[string]map[string]string{}, pinMu: map[string]*sync.Mutex{},
		snaps: snapUse{inUse: map[string]int{}, removing: map[string]chan struct{}{}, taken: map[string]string{}}}, nil
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

// build builds the template's images. A reference pins names a digest for (a snapshot's, Manifest.Pins) is built
// from that digest; any other is pinned to the digest its first boot of template t on this host resolved it to, so
// a template's tag is resolved once and every VM of it runs one image, whatever the tag does afterwards.
func (m *Machines) build(ctx context.Context, t link.Template, pins map[string]string) (map[string]image.Rootfs, error) {
	roots := map[string]image.Rootfs{}
	for _, c := range t.Containers {
		if _, ok := roots[c.Image]; ok {
			continue
		}
		var r image.Rootfs
		var err error
		if d, ok := pins[c.Image]; ok {
			r, err = m.pinned(ctx, c.Image, d)
		} else {
			r, err = m.first(ctx, t.UID, c.Image)
		}
		if err != nil {
			return nil, fmt.Errorf("machine: container %q: %w", c.Name, err)
		}
		roots[c.Image] = r
	}
	return roots, nil
}

// pinned builds ref's image from digest.
func (m *Machines) pinned(ctx context.Context, ref, digest string) (image.Rootfs, error) {
	p, err := image.Pin(ref, digest)
	if err != nil {
		return image.Rootfs{}, err
	}
	r, err := m.cfg.Images.Get(ctx, p)
	if err != nil {
		return image.Rootfs{}, err
	}
	if r.Index == "" {
		r.Index = digest
	}
	return r, nil
}

// first builds ref's image for template uid: from the digest the template's first boot here pinned it to, or,
// on that first boot, from what the reference resolves to now, which becomes the template's pin.
func (m *Machines) first(ctx context.Context, uid, ref string) (image.Rootfs, error) {
	key := uid + "\x00" + ref
	m.mu.Lock()
	l, ok := m.pinMu[key]
	if !ok {
		l = &sync.Mutex{}
		m.pinMu[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	m.mu.Lock()
	d, ok := m.pins[uid][ref]
	m.mu.Unlock()
	if ok {
		return m.pinned(ctx, ref, d)
	}
	r, err := m.cfg.Images.Get(ctx, ref)
	if err != nil {
		return image.Rootfs{}, err
	}
	if r.Index == "" {
		r.Index = r.Digest
	}
	if image.ValidDigest(r.Index) {
		m.mu.Lock()
		if m.pins[uid] == nil {
			m.pins[uid] = map[string]string{}
		}
		m.pins[uid][ref] = r.Index
		m.mu.Unlock()
	}
	return r, nil
}

// prepare builds the template's images (build: pins are a snapshot's), leases the VM its link (Config.Network; want
// is a MEMORY restore's) and lays out a fresh share for vmID with the plan and empty volumes. fill, if set, runs on
// the share before the volumes are laid (a restore unpacks its volume tree there). A failure other than
// errShareTaken and errLinkTaken leaves a lease the caller releases (unlease).
func (m *Machines) prepare(ctx context.Context, vmID string, t link.Template, pins map[string]string, fill func(*os.Root) error, want *initd.Network) (vmm.Spec, map[string]image.Rootfs, error) {
	if len(t.Containers) == 0 {
		return vmm.Spec{}, nil, fmt.Errorf("machine: template %q has no container", t.UID)
	}
	roots, err := m.build(ctx, t, pins)
	if err != nil {
		return vmm.Spec{}, nil, err
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
	return m.spec(vmID, t, disks), roots, nil
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
	spec, roots, err := m.prepare(ctx, id, t, nil, nil, nil)
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
	m.tpl[id], m.images[id] = t, roots
	m.mu.Unlock()
	return nil
}

// Snapshot has the guest flush what it buffered (Config.Flush), takes a snapshot of the actor's VM at f and writes
// its bundle to w. With stop (a suspend), the VM is stopped right after the VMM's snapshot and before the volume tree
// is read, so the tree is the one the actor left and the guest writes nothing more, and its link is taken down
// (verified; the lease stays for Restart); if writing the bundle then fails, Restart boots it again from its share.
// A MEMORY bundle records the VM's link, which its memory keeps, and carries the VMM's snapshot files when the VMM
// is a vmm.SnapshotCarrier.
func (m *Machines) Snapshot(ctx context.Context, id string, f vmm.Fidelity, stop bool, w io.Writer) (_ vmm.Snapshot, err error) {
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
	if err := m.flush(ctx, vmID, m.spec(vmID, t, nil).MemoryMiB); err != nil {
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
	defer func() {
		if err != nil {
			// No bundle holds it and nothing will restore from it.
			m.dropSnapshot(ctx, snap.ID)
		}
	}()
	if !stop {
		// The VM runs on from its pause, its clock behind by the pause. Logged and not refused: the snapshot is whole,
		// and failing it would not set the clock either.
		if err := m.setClock(ctx, vmID, "snapshot "+id); err != nil {
			log.Printf("machine: snapshot %s: the guest's wall clock is NOT set and is behind by the snapshot's pause: %v", id, err)
		}
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
	// The images the VM runs, not what its references resolve to now: a resume, fork or move runs these again.
	m.mu.Lock()
	for ref, r := range m.images[vmID] {
		if man.Pins == nil {
			man.Pins = map[string]string{}
		}
		man.Images[ref], man.Pins[ref] = r.Digest, r.Index
	}
	m.mu.Unlock()
	if c, ok := m.cfg.VMM.(vmm.SnapshotCarrier); ok && snap.Fidelity == vmm.FidelityMemory {
		// The memory goes into the bundle: what herd-api verifies in the store is then all a restore on another host
		// of the family needs, and the snapshot outlives this host.
		files, err := c.ExportSnapshot(ctx, snap)
		if err != nil {
			return vmm.Snapshot{}, fmt.Errorf("machine: snapshot %s: carrying its memory: %w", id, err)
		}
		man.Memory = files
	}
	if err := writeBundle(w, man, filepath.Join(m.cfg.ShareDir, vmID)); err != nil {
		return vmm.Snapshot{}, err
	}
	m.taken(snap.ID, id)
	return snap, nil
}

// Memory is how many bytes of memory the actor's VM has; false when this host runs no VM of id.
func (m *Machines) Memory(id string) (int64, bool) {
	vmID := m.resolve(id)
	m.mu.Lock()
	t, ok := m.tpl[vmID]
	m.mu.Unlock()
	if !ok {
		return 0, false
	}
	return int64(m.spec(vmID, t, nil).MemoryMiB) << 20, true
}

// flush has the guest write back what it buffered, within the flush timeout for a VM of memoryMiB.
func (m *Machines) flush(ctx context.Context, vmID string, memoryMiB int) error {
	if m.cfg.Flush == nil {
		return nil
	}
	d := m.cfg.FlushTimeout
	if d <= 0 {
		d = FlushTimeoutFor(memoryMiB)
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return m.cfg.Flush.Flush(ctx, vmID)
}

// Restart cold-boots a VM that Snapshot stopped, from its share as it is, on the link it still holds and with the
// images it ran.
func (m *Machines) Restart(ctx context.Context, id string) error {
	vmID := m.resolve(id)
	m.mu.Lock()
	t, ok := m.tpl[vmID]
	roots := m.images[vmID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("machine: restart %s: %w", id, vmm.ErrNotFound)
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
	_ = os.Remove(filepath.Join(m.cfg.ShareDir, vmID, initd.ExitPath))
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
// a MEMORY bundle restores only in its own restore family, under the actor's own VM id and on the link it was
// taken on where this host can give it (else it cold-boots), from the snapshot files the bundle carries (vmm.SnapshotCarrier) or, for a bundle without them, from the
// VMM's own snapshot directory. A MEMORY bundle of another family is refused before anything on this host is touched.
func (m *Machines) Restore(ctx context.Context, id string, r io.Reader) error {
	_, err := m.RestoreSnapshot(ctx, id, r)
	return err
}

// RestoreSnapshot is Restore, and answers the snapshot the bundle holds (when it could read it). The actor's resume
// from its own snapshot uses the host's copy of it up: the copy is removed once the actor runs again (PLT-374).
func (m *Machines) RestoreSnapshot(ctx context.Context, id string, r io.Reader) (vmm.Snapshot, error) {
	tr := tar.NewReader(r)
	man, err := readManifest(tr)
	if err != nil {
		return vmm.Snapshot{}, err
	}
	done := m.useSnapshot(man.Snapshot.ID)
	err = m.restore(ctx, id, tr, man)
	done()
	if err == nil && m.ownSnapshot(id, man.Snapshot) {
		m.dropSnapshot(ctx, man.Snapshot.ID)
	}
	return man.Snapshot, err
}

func (m *Machines) restore(ctx context.Context, id string, tr *tar.Reader, man Manifest) error {
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
		want = man.Network
		if (want != nil) != (m.cfg.Network != nil) {
			// The memory's devices are not this host's: a guest that had a link cannot keep it on a host that gives VMs
			// none (Cloud Hypervisor refuses a device set other than the snapshot's), and one without a link would not
			// see the link this host gives every VM. The actor cold-boots from the bundle's volume tree instead.
			log.Printf("machine: restore %s: the memory of %s was taken with link %v, this host gives VMs links: %v: cold boot from its volumes", id, snap.VMID, want != nil, m.cfg.Network != nil)
			cold, want = true, nil
		}
	default:
		return fmt.Errorf("machine: restore %s at %v: %w", id, snap.Fidelity, vmm.ErrFidelityUnsupported)
	}
	fill := func(root *os.Root) error {
		var keep func(func() (vmm.SnapshotFile, io.Reader, error)) error
		if c, ok := m.cfg.VMM.(vmm.SnapshotCarrier); ok && snap.Fidelity == vmm.FidelityMemory && len(man.Memory) > 0 && !cold {
			keep = func(next func() (vmm.SnapshotFile, io.Reader, error)) error {
				if err := c.ImportSnapshot(ctx, snap, next); err != nil {
					return fmt.Errorf("machine: restore %s: the memory the bundle carries: %w", id, err)
				}
				return nil
			}
		}
		if err := readMemory(tr, man, keep); err != nil {
			return err
		}
		return extractVolumes(tr, root, man.Template.Volumes)
	}
	pins := m.bundlePins(ctx, id, man)
	spec, roots, err := m.prepare(ctx, vmID, man.Template, pins, fill, want)
	if errors.Is(err, errLinkTaken) {
		// The link this memory keeps (the guest's address and MAC) is another VM's on this host: the snapshot's
		// source running here, or another fork of one MEMORY tag restored before or at the same time. One address
		// cannot run twice on one host, so this actor cold-boots from the bundle's volume tree on a link of its own,
		// as a VOLUMES restore does. The lease is the claim, so of two forks at once exactly one gets the memory;
		// the bundle is still unread, since fill runs only after the lease and the share.
		log.Printf("machine: restore %s: the memory of %s cannot run here (%v): cold boot from its volumes on a link of its own", id, snap.VMID, err)
		cold = true
		spec, roots, err = m.prepare(ctx, vmID, man.Template, pins, fill, nil)
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
		if errors.Is(err, vmm.ErrNotFound) && len(man.Memory) == 0 {
			// Not a cold boot in its place: the client asked for the memory, and a VOLUMES restore is no MEMORY one.
			err = fmt.Errorf("machine: restore %s: the bundle carries no memory files (written by a VMM that keeps them, or before bundle version 2), and this host does not hold them: %w", id, err)
		}
	} else if err == nil {
		_, err = m.cfg.VMM.Boot(ctx, spec)
	}
	if err == nil && snap.Fidelity == vmm.FidelityMemory && !cold {
		// The memory runs on from the snapshot's clock. The host's is set before Restore answers, and so before herdlet lets
		// anything reach the VM; a guest whose clock cannot be set is stopped rather than run a session on a stale clock.
		if cerr := m.setClock(ctx, vmID, "restore "+id); cerr != nil {
			if p, derr := m.cfg.VMM.Delete(ctx, vmID); p != vmm.PresenceGone {
				// It may still run: it keeps its share and link and is recorded, so a delete removes it.
				m.mu.Lock()
				m.tpl[vmID], m.images[vmID] = man.Template, roots
				m.mu.Unlock()
				m.Adopt(id, vmID)
				return fmt.Errorf("machine: restore %s: the guest's wall clock could not be set (%v), and stopping the VM: %s: %w", id, cerr, p, derr)
			}
			err = fmt.Errorf("machine: restore %s: the guest's wall clock could not be set, so the VM was stopped rather than run on its snapshot's clock: %v", id, cerr)
		}
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
	m.tpl[vmID], m.images[vmID] = man.Template, roots
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
	delete(m.images, vmID)
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

// bundlePins is what a restore builds the bundle's images from: the digests its snapshot's VM ran (Manifest.Pins).
// A bundle written before herd pinned images carries only platform manifest digests (Manifest.Images), which name
// the same images on a host of the producer's architecture; on any other host the references are resolved again,
// and that is logged.
func (m *Machines) bundlePins(ctx context.Context, id string, man Manifest) map[string]string {
	if len(man.Pins) > 0 {
		return man.Pins
	}
	if len(man.Images) == 0 {
		return nil
	}
	if caps, err := m.cfg.VMM.Capabilities(ctx); err == nil && caps.Arch == man.Arch {
		return man.Images
	}
	log.Printf("machine: restore %s: the bundle pins no image for this host's architecture (it was written on %s): resolving its references again", id, man.Arch)
	return nil
}

// ImagePaths is every image file a VM this host holds runs on (image.Cache.InUse): the cache never removes one.
func (m *Machines) ImagePaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, roots := range m.images {
		for _, r := range roots {
			out = append(out, r.Path)
		}
	}
	return out
}

// Images is the image the actor's VM runs per reference of its template (nil for a VM this host does not hold).
func (m *Machines) Images(id string) map[string]image.Rootfs {
	vmID := m.resolve(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.images[vmID] == nil {
		return nil
	}
	return maps.Clone(m.images[vmID])
}
