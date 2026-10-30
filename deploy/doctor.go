package deploy

import (
	"bytes"
	"errors"
	"strings"

	"fmt"
	"github.com/everdict/herd/guest"
	"github.com/everdict/herd/internal/plist"
	"os"
	"os/exec"
	"runtime"
)

// Check is one line of `herdlet doctor`.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Host is what doctor looks at; the real one reads this machine.
type Host struct {
	GOOS, GOARCH string
	// VZ reports whether this binary can drive Virtualization.framework (a cgo darwin/arm64 build).
	VZ       bool
	OpenRW   func(path string) error
	LookPath func(name string) (string, error)
	Stat     func(path string) error
	// Entitlements returns the entitlements this binary is signed with (codesign -d --entitlements - --xml); nil
	// skips the check.
	Entitlements func() ([]byte, error)
	// KernelArch reads a guest kernel's architecture from its boot header (guest.KernelArch); nil skips the check.
	KernelArch func(path string) (string, error)
}

// ThisHost is the machine herdlet runs on.
func ThisHost() Host {
	return Host{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, VZ: vzBuilt(),
		OpenRW: func(p string) error {
			f, err := os.OpenFile(p, os.O_RDWR, 0)
			if err == nil {
				f.Close()
			}
			return err
		},
		LookPath: exec.LookPath,
		Stat:     func(p string) error { _, err := os.Stat(p); return err },
		Entitlements: func() ([]byte, error) {
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			var out, errb bytes.Buffer
			cmd := exec.Command("/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", exe)
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("codesign: %w: %s", err, strings.TrimSpace(errb.String()))
			}
			return out.Bytes(), nil
		},
		KernelArch: guest.KernelArch,
	}
}

// Doctor says whether this host can run herd actors with h's configuration, and why not (R-D7): a Linux host needs
// KVM, Cloud Hypervisor and virtiofsd; a Mac needs Apple Silicon and a herdlet built with cgo; both need the guest
// kernel and initramfs.
func Doctor(h *Herdlet, host Host) []Check {
	var cs []Check
	add := func(name string, err error, okDetail string) {
		if err != nil {
			cs = append(cs, Check{Name: name, Detail: err.Error()})
			return
		}
		cs = append(cs, Check{Name: name, OK: true, Detail: okDetail})
	}
	switch host.GOOS {
	case "linux":
		var archErr error
		if host.GOARCH != "amd64" && host.GOARCH != "arm64" {
			archErr = fmt.Errorf("herd runs on amd64 and arm64, not %s", host.GOARCH)
		}
		add("arch", archErr, host.GOARCH)
		if err := host.OpenRW("/dev/kvm"); err != nil {
			add("kvm", fmt.Errorf("/dev/kvm cannot be opened read-write (%v): enable virtualization and load kvm, and run herdlet as a user in the kvm group", err), "")
		} else {
			add("kvm", nil, "/dev/kvm")
		}
		// The hypervisor, virtiofsd, and ip(8) and nft(8), which make each VM's TAP and egress table (ADR A-18).
		for _, b := range []struct{ name, path string }{{"cloud-hypervisor", h.CHBinary}, {"virtiofsd", h.Virtiofsd}, {"ip", "ip"}, {"nft", "nft"}} {
			p, err := host.LookPath(b.path)
			if err != nil {
				err = fmt.Errorf("%s not found (%v): install it or pass its path", b.path, err)
			}
			add(b.name, err, p)
		}
	case "darwin":
		var archErr error
		if host.GOARCH != "arm64" {
			archErr = fmt.Errorf("herd runs Linux microVMs on Apple Silicon only, not %s", host.GOARCH)
		}
		add("arch", archErr, host.GOARCH)
		var vzErr error
		if !host.VZ {
			vzErr = fmt.Errorf("this herdlet was built without cgo, so it cannot drive Virtualization.framework; use the darwin/arm64 release built on a Mac")
		}
		add("virtualization.framework", vzErr, "linked")
		if host.Entitlements != nil {
			ent, err := host.Entitlements()
			if err == nil {
				err = HasVirtualization(ent)
			}
			// Without it Virtualization.framework refuses every VM; the release build signs it (scripts/release.sh).
			add("entitlement", err, VirtualizationEntitlement)
		}
	default:
		add("os", fmt.Errorf("herdlet runs on Linux (KVM) and macOS (Apple Silicon), not %s", host.GOOS), "")
	}
	for _, f := range []struct{ name, path string }{{"kernel", h.Kernel}, {"initrd", h.Initrd}} {
		err := host.Stat(f.path)
		if err != nil {
			err = fmt.Errorf("guest %s %s: %v", f.name, f.path, err)
		}
		add(f.name, err, f.path)
	}
	// One build makes both architectures' guests (guest/build.sh all): the host must boot its own, never the other.
	if host.KernelArch != nil && host.Stat(h.Kernel) == nil {
		arch, err := host.KernelArch(h.Kernel)
		if err == nil && arch != host.GOARCH {
			err = fmt.Errorf("guest kernel %s is %s, this host is %s: install herd-guest-%s from guest/build.sh all", h.Kernel, arch, host.GOARCH, host.GOARCH)
		}
		add("kernel arch", err, arch)
	}
	return cs
}

// VirtualizationEntitlement is what Virtualization.framework requires of the process that makes VMs.
const VirtualizationEntitlement = "com.apple.security.virtualization"

// HasVirtualization checks an entitlements plist (codesign -d --entitlements - --xml) grants
// com.apple.security.virtualization.
func HasVirtualization(entitlements []byte) error {
	if len(strings.TrimSpace(string(entitlements))) == 0 {
		return fmt.Errorf("this herdlet is not signed with entitlements; sign it with %s (scripts/release.sh does)", VirtualizationEntitlement)
	}
	v, err := plist.Decode(entitlements)
	if err != nil {
		return fmt.Errorf("entitlements: %w", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return errors.New("entitlements are not a dict")
	}
	if on, _ := d[VirtualizationEntitlement].(bool); !on {
		return fmt.Errorf("this herdlet is not signed with %s, so Virtualization.framework would refuse every VM; sign it with vmm/vz/vz.entitlements (scripts/release.sh does)", VirtualizationEntitlement)
	}
	return nil
}
