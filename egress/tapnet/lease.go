// Package tapnet is the Ubuntu half of a VM's network as herdlet runs it (ADR 0001 A-18): each VM leases a /30 of a
// host pool and a TAP named after it (Leases), gets its own nftables table from egress/nft loaded before the link
// comes up, and is bound to its actor in the bindings file herd-egress reads, so every flow from the VM's address is
// decided under that actor's EgressPolicy (Network). Teardown is verified: the TAP and the table are observed
// absent, and the binding is read back gone, before a VM's network answers gone.
//
// Running commands is nft.Host's (faked in tests); the real-host test (HERD_TAP_REAL=1) boots a Cloud Hypervisor
// VM through it.
package tapnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
)

// ErrTaken is answered for a lease that must be one particular link (a MEMORY restore's) when another VM on this
// host holds it, or the pool cannot hold it.
var ErrTaken = errors.New("tapnet: the link is taken on this host")

// DefaultPool is the pool VM links are leased from.
var DefaultPool = netip.MustParsePrefix("10.88.0.0/16")

// Lease is one VM's link: slot i of the pool is the /30 at pool + 4i, the host at .1 and the VM at .2 of it.
type Lease struct {
	Slot int
	// TAP is the host's TAP device, herd<slot>.
	TAP string
	// MAC is the VM's address on it, 02:68:64 (locally administered, "hd") and the slot.
	MAC string
	// Host is the host's address on the TAP with the /30.
	Host netip.Prefix
	// VM is the VM's address.
	VM netip.Addr
}

// Leases hands out pool slots to VM ids and keeps them in a file, so a herdlet that restarts while its VMs run gives
// no slot twice. A VM id's first choice is a hash of the id, so a VM moved or restored on another host with the same
// pool usually gets the link it had.
type Leases struct {
	// Path is the lease file.
	Path string
	// Pool is an IPv4 prefix of at most /30; zero means DefaultPool.
	Pool netip.Prefix

	mu     sync.Mutex
	loaded bool
	slots  map[string]int
}

type leaseFile struct {
	Pool   string         `json:"pool"`
	Leases map[string]int `json:"leases"`
}

func (l *Leases) pool() (netip.Prefix, int, error) {
	p := l.Pool
	if !p.IsValid() {
		p = DefaultPool
	}
	if !p.Addr().Is4() || p.Bits() > 30 || p.Masked() != p {
		return netip.Prefix{}, 0, fmt.Errorf("tapnet: pool %v must be an IPv4 network of at most /30", p)
	}
	return p, 1 << (30 - p.Bits()), nil
}

func (l *Leases) load() error {
	if l.loaded {
		return nil
	}
	l.slots = map[string]int{}
	b, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		l.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("tapnet: leases: %w", err)
	}
	var f leaseFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("tapnet: leases %s: %w", l.Path, err)
	}
	p, _, err := l.pool()
	if err != nil {
		return err
	}
	if len(f.Leases) > 0 && f.Pool != p.String() {
		// The slots name addresses of the old pool, which VMs that still run hold.
		return fmt.Errorf("tapnet: leases %s are of pool %s, not %s: delete the host's VMs before changing the pool", l.Path, f.Pool, p)
	}
	for id, s := range f.Leases {
		l.slots[id] = s
	}
	l.loaded = true
	return nil
}

func (l *Leases) save() error {
	p, _, err := l.pool()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(leaseFile{Pool: p.String(), Leases: l.slots}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(l.Path, b)
}

// writeAtomic replaces path with b: written and synced under a temporary name, then renamed over it.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("tapnet: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("tapnet: %w", err)
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("tapnet: writing %s: %w", path, err)
	}
	return nil
}

// lease is slot s of pool p.
func lease(p netip.Prefix, s int) Lease {
	b := p.Addr().As4()
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	at := func(off uint32) netip.Addr {
		v := base + uint32(s)*4 + off
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	return Lease{Slot: s, TAP: fmt.Sprintf("herd%d", s), MAC: fmt.Sprintf("02:68:64:%02x:%02x:%02x", byte(s>>16), byte(s>>8), byte(s)),
		Host: netip.PrefixFrom(at(1), 30), VM: at(2)}
}

func preferred(id string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(n))
}

// Get answers the VM's lease, leasing it a slot if it holds none: its preferred slot, else the next free one.
func (l *Leases) Get(id string) (Lease, error) {
	return l.get(id, -1)
}

// Claim answers the VM's lease, which must be want: the VM already holds it, or it is free and is leased to the VM.
// Anything else is ErrTaken.
func (l *Leases) Claim(id string, want Lease) (Lease, error) {
	p, n, err := l.pool()
	if err != nil {
		return Lease{}, err
	}
	if want.Slot < 0 || want.Slot >= n || lease(p, want.Slot) != want {
		return Lease{}, fmt.Errorf("%w: %+v is not a link of pool %v", ErrTaken, want, p)
	}
	return l.get(id, want.Slot)
}

func (l *Leases) get(id string, want int) (Lease, error) {
	p, n, err := l.pool()
	if err != nil {
		return Lease{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return Lease{}, err
	}
	if s, ok := l.slots[id]; ok {
		if want >= 0 && s != want {
			return Lease{}, fmt.Errorf("%w: %s holds %s here, not %s", ErrTaken, id, lease(p, s).VM, lease(p, want).VM)
		}
		return lease(p, s), nil
	}
	held := map[int]string{}
	for other, s := range l.slots {
		held[s] = other
	}
	s := want
	if s >= 0 {
		if other, ok := held[s]; ok {
			return Lease{}, fmt.Errorf("%w: %s is %s's", ErrTaken, lease(p, s).VM, other)
		}
	} else {
		start := preferred(id, n)
		for i := 0; i < n; i++ {
			if _, ok := held[(start+i)%n]; !ok {
				s = (start + i) % n
				break
			}
		}
		if s < 0 {
			return Lease{}, fmt.Errorf("tapnet: pool %v is full (%d links)", p, n)
		}
	}
	l.slots[id] = s
	if err := l.save(); err != nil {
		delete(l.slots, id)
		return Lease{}, err
	}
	return lease(p, s), nil
}

// Held answers the VM's lease, if it holds one.
func (l *Leases) Held(id string) (Lease, bool, error) {
	p, _, err := l.pool()
	if err != nil {
		return Lease{}, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return Lease{}, false, err
	}
	s, ok := l.slots[id]
	if !ok {
		return Lease{}, false, nil
	}
	return lease(p, s), true, nil
}

// Release frees the VM's slot; releasing a VM that holds none is not an error.
func (l *Leases) Release(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	s, ok := l.slots[id]
	if !ok {
		return nil
	}
	delete(l.slots, id)
	if err := l.save(); err != nil {
		l.slots[id] = s
		return err
	}
	return nil
}
