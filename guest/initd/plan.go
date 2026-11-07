// Package initd is what herd-guest does as PID 1 of the microVM, in order: mount the guest's pseudo filesystems,
// mount the virtio-fs share, configure the VM's network link to herd-egress if the plan has one (address, default
// route, and the resolver and CA every container is given), create every DurableDir volume as a subdirectory of the
// share, then — only after all volumes exist — write each container's OCI bundle and start it. The plan (which
// containers, which volumes) is written by herdlet from the ActorTemplate into the share at .herd/actor.json.
package initd

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"path"
	"regexp"
	"strings"

	"github.com/everdict/herd/guest/oci"
)

// PlanPath is the plan's path relative to the share.
const PlanPath = ".herd/actor.json"

// Plan mirrors the parts of a Substrate ActorTemplate the guest acts on, with images already resolved to block
// devices by herdlet.
type Plan struct {
	Volumes    []Volume    `json:"volumes"`
	Containers []Container `json:"containers"`
	// Network, if set, is the VM's one NIC: herd-guest configures it before any container starts.
	Network *Network `json:"network,omitempty"`
}

// Network is the VM's link to herd-egress as herdlet leased it (ADR A-18 on Ubuntu: the VM's address on its TAP's
// /30; A-21 on a Mac: the userspace stack's fixed link). Every TCP connection and DNS query leaves through
// herd-egress whatever the VM addresses, so the guest needs only an address, a default route and a resolver, plus
// herd-egress's CA for the connections it intercepts.
type Network struct {
	// MAC names the interface; empty means the guest's only non-loopback interface.
	MAC string `json:"mac,omitempty"`
	// Address is the VM's address with the link's prefix length, e.g. 10.88.0.2/30.
	Address string `json:"address"`
	// Gateway is the default route's next hop: the host's side of the link.
	Gateway string `json:"gateway"`
	// DNS are the resolvers every container gets in /etc/resolv.conf.
	DNS []string `json:"dns,omitempty"`
	// CA is herd-egress's CA certificate (PEM), added to every container's trust store.
	CA string `json:"ca,omitempty"`
}

// Volume is a DurableDir volume: a directory of the share named after the volume.
type Volume struct {
	Name string `json:"name"`
}

// Container is one OCI container of the template.
type Container struct {
	Name string `json:"name"`
	// Image is the container's read-only root image (erofs or ext4) as a block device; a writable layer goes on top.
	Image   Image    `json:"image"`
	Command []string `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []EnvVar `json:"env,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
	// User is the image's USER (name or uid, optionally :group or :gid), resolved in the container's rootfs as Docker
	// does (oci.ResolveUser); empty is root.
	User         string        `json:"user,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
	// CapAdd and CapDrop are the template's security_context.capabilities, as it wrote them (oci.CheckCapabilities):
	// they change Docker's default set of the container's process and of every exec into it, and drop wins.
	CapAdd  []string `json:"capAdd,omitempty"`
	CapDrop []string `json:"capDrop,omitempty"`
}

type Image struct {
	Device string `json:"device"`
	FSType string `json:"fsType"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// cleanAbs is Substrate's VolumeMount.mount_path rule: starts with '/', is not '/', and has no '..', '.', '//',
// trailing '/', ':' or control characters.
func cleanAbs(p string) bool {
	if !strings.HasPrefix(p, "/") || p == "/" || path.Clean(p) != p || strings.ContainsAny(p, ":") {
		return false
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// ParsePlan decodes and validates a plan.
func ParsePlan(b []byte) (*Plan, error) {
	var p Plan
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("initd: plan: %w", err)
	}
	return &p, p.Validate()
}

// Validate checks names and references: a volume name is a DNS label (so it is one path element of the share and
// cannot be ".herd", "..", or contain '/'); each mount names a declared volume at a clean absolute path.
func (p *Plan) Validate() error {
	vols := map[string]bool{}
	for _, v := range p.Volumes {
		if !dnsLabel.MatchString(v.Name) {
			return fmt.Errorf("initd: volume name %q is not a DNS label", v.Name)
		}
		if vols[v.Name] {
			return fmt.Errorf("initd: volume %q declared twice", v.Name)
		}
		vols[v.Name] = true
	}
	if len(p.Containers) == 0 {
		return fmt.Errorf("initd: plan has no container")
	}
	names := map[string]bool{}
	for _, c := range p.Containers {
		if !dnsLabel.MatchString(c.Name) {
			return fmt.Errorf("initd: container name %q is not a DNS label", c.Name)
		}
		if names[c.Name] {
			return fmt.Errorf("initd: container %q declared twice", c.Name)
		}
		names[c.Name] = true
		if len(c.Command)+len(c.Args) == 0 {
			return fmt.Errorf("initd: container %q has no command", c.Name)
		}
		if !strings.HasPrefix(c.Image.Device, "/dev/") || c.Image.FSType != "erofs" && c.Image.FSType != "ext4" {
			return fmt.Errorf("initd: container %q: image must be an erofs or ext4 block device under /dev", c.Name)
		}
		if c.Workdir != "" && !cleanAbs(c.Workdir) {
			return fmt.Errorf("initd: container %q: workdir %q is not a clean absolute path", c.Name, c.Workdir)
		}
		if err := oci.CheckCapabilities(c.CapAdd, c.CapDrop); err != nil {
			return fmt.Errorf("initd: container %q: %w", c.Name, err)
		}
		at := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if !vols[m.Name] {
				return fmt.Errorf("initd: container %q mounts undeclared volume %q", c.Name, m.Name)
			}
			if !cleanAbs(m.MountPath) || strings.HasPrefix(m.MountPath, "/.herd") {
				return fmt.Errorf("initd: container %q: mount path %q is not a clean absolute path", c.Name, m.MountPath)
			}
			if at[m.MountPath] {
				return fmt.Errorf("initd: container %q: two mounts at %s", c.Name, m.MountPath)
			}
			at[m.MountPath] = true
		}
	}
	if p.Network != nil {
		return p.Network.Validate()
	}
	return nil
}

// Validate checks the link: Address is an IPv4 host address (neither the network's nor the broadcast address) on a
// /30 or wider, Gateway another host address of that prefix, every DNS entry an IP, MAC a hardware address, and CA,
// if set, one or more PEM certificates and nothing else.
func (n *Network) Validate() error {
	pfx, err := netip.ParsePrefix(n.Address)
	if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 1 || pfx.Bits() > 30 {
		return fmt.Errorf("initd: network: address %q is not an IPv4 address on a /30 or wider", n.Address)
	}
	host := func(a netip.Addr) bool { return a != pfx.Masked().Addr() && a != broadcast(pfx) }
	if !host(pfx.Addr()) {
		return fmt.Errorf("initd: network: address %q is its network's or broadcast address", n.Address)
	}
	gw, err := netip.ParseAddr(n.Gateway)
	if err != nil || !gw.Is4() || !pfx.Contains(gw) || !host(gw) || gw == pfx.Addr() {
		return fmt.Errorf("initd: network: gateway %q is not another host address of %s", n.Gateway, pfx.Masked())
	}
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(d); err != nil || a.Zone() != "" {
			return fmt.Errorf("initd: network: DNS server %q is not an IP address", d)
		}
	}
	if n.MAC != "" {
		if _, err := net.ParseMAC(n.MAC); err != nil {
			return fmt.Errorf("initd: network: mac %q: %w", n.MAC, err)
		}
	}
	if n.CA != "" {
		rest, certs := []byte(n.CA), 0
		for {
			var b *pem.Block
			if b, rest = pem.Decode(rest); b == nil {
				break
			}
			if b.Type != "CERTIFICATE" {
				return fmt.Errorf("initd: network: CA holds a PEM %s block, not a CERTIFICATE", b.Type)
			}
			if _, err := x509.ParseCertificate(b.Bytes); err != nil {
				return fmt.Errorf("initd: network: CA: %w", err)
			}
			certs++
		}
		if certs == 0 || strings.TrimSpace(string(rest)) != "" {
			return fmt.Errorf("initd: network: CA is not a PEM certificate bundle")
		}
	}
	return nil
}

// broadcast is p's last address (p IPv4).
func broadcast(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		a[i/8] |= 0x80 >> (i % 8)
	}
	return netip.AddrFrom4(a)
}
