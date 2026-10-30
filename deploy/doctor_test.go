package deploy_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/everdict/herd/deploy"
)

func host(goos, arch string, kvm error, vz bool) deploy.Host {
	return deploy.Host{GOOS: goos, GOARCH: arch, VZ: vz,
		OpenRW:   func(string) error { return kvm },
		LookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		Stat:     func(string) error { return nil },
	}
}

func failed(cs []deploy.Check) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		if !c.OK {
			m[c.Name] = c.Detail
		}
	}
	return m
}

// TestDoctorSaysWhy: a Linux host without KVM and a Mac whose herdlet cannot reach Virtualization.framework cannot
// join, and doctor says why (R-D7); a ready host passes.
func TestDoctorSaysWhy(t *testing.T) {
	linux := &deploy.Herdlet{CHBinary: "cloud-hypervisor", Virtiofsd: "virtiofsd", Kernel: "k", Initrd: "i"}
	mac := &deploy.Herdlet{Kernel: "k", Initrd: "i"}
	if f := failed(deploy.Doctor(linux, host("linux", "amd64", nil, false))); len(f) != 0 {
		t.Errorf("ready Linux host: %v", f)
	}
	if f := failed(deploy.Doctor(linux, host("linux", "amd64", errors.New("no such file"), false))); !strings.Contains(f["kvm"], "/dev/kvm") {
		t.Errorf("Linux host without KVM: %v", f)
	}
	if f := failed(deploy.Doctor(mac, host("darwin", "arm64", nil, true))); len(f) != 0 {
		t.Errorf("ready Mac: %v", f)
	}
	if f := failed(deploy.Doctor(mac, host("darwin", "arm64", nil, false))); !strings.Contains(f["virtualization.framework"], "cgo") {
		t.Errorf("Mac with a non-cgo herdlet: %v", f)
	}
	if f := failed(deploy.Doctor(mac, host("darwin", "amd64", nil, true))); f["arch"] == "" {
		t.Errorf("Intel Mac: %v", f)
	}
}

const (
	granted = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`
	denied  = `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>com.apple.security.virtualization</key><false/></dict></plist>`
	other   = `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>com.apple.security.cs.allow-jit</key><true/></dict></plist>`
)

// TestDoctorRefusesAnUnentitledMacHerdlet (criterion launchd-install): on a Mac, doctor — and so the preflight of
// `herdlet join` and `herdlet run`, which the launchd daemon runs — fails a herdlet not signed with
// com.apple.security.virtualization (unsigned, entitlement false, other entitlements only, codesign failing), and
// passes one signed with it, as scripts/release.sh signs it. Without the check the daemon would start a herdlet whose
// every VM Virtualization.framework refuses: red.
func TestDoctorRefusesAnUnentitledMacHerdlet(t *testing.T) {
	mac := &deploy.Herdlet{Kernel: "k", Initrd: "i"}
	signed := func(ent string, err error) deploy.Host {
		h := host("darwin", "arm64", nil, true)
		h.Entitlements = func() ([]byte, error) { return []byte(ent), err }
		return h
	}
	if f := failed(deploy.Doctor(mac, signed(granted, nil))); len(f) != 0 {
		t.Errorf("signed Mac herdlet: %v", f)
	}
	for name, h := range map[string]deploy.Host{
		"unsigned":                signed("", nil),
		"entitlement false":       signed(denied, nil),
		"other entitlements only": signed(other, nil),
		"codesign fails":          signed("", errors.New("codesign: code object is not signed at all")),
	} {
		if f := failed(deploy.Doctor(mac, h)); f["entitlement"] == "" {
			t.Errorf("%s: doctor passed it: %v", name, f)
		}
	}
	b, err := os.ReadFile("../vmm/vz/vz.entitlements")
	if err != nil {
		t.Fatal(err)
	}
	if err := deploy.HasVirtualization(b); err != nil {
		t.Errorf("vmm/vz/vz.entitlements: %v", err)
	}
	rel, err := os.ReadFile("../scripts/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rel), `codesign --force --entitlements "$here/vmm/vz/vz.entitlements"`) {
		t.Errorf("scripts/release.sh does not sign the darwin herdlet with vmm/vz/vz.entitlements")
	}
}

// TestDoctorRefusesTheOtherArchsKernel (criterion multi-arch): a host whose -kernel is the other architecture's
// boot image fails doctor (and so cannot join or run): an arm64 Mac handed an amd64 bzImage, an amd64 Linux host an
// arm64 Image. Its own architecture's kernel passes.
func TestDoctorRefusesTheOtherArchsKernel(t *testing.T) {
	for _, tc := range []struct {
		goos, arch, kernel string
		vz                 bool
		ok                 bool
	}{
		{"darwin", "arm64", "arm64", true, true},
		{"darwin", "arm64", "amd64", true, false},
		{"linux", "amd64", "amd64", false, true},
		{"linux", "amd64", "arm64", false, false},
		{"linux", "arm64", "arm64", false, true},
	} {
		h := host(tc.goos, tc.arch, nil, tc.vz)
		h.KernelArch = func(string) (string, error) { return tc.kernel, nil }
		hl := &deploy.Herdlet{CHBinary: "cloud-hypervisor", Virtiofsd: "virtiofsd", Kernel: "k", Initrd: "i"}
		f := failed(deploy.Doctor(hl, h))
		if (f["kernel arch"] == "") != tc.ok {
			t.Errorf("%s/%s host with a %s kernel: %v", tc.goos, tc.arch, tc.kernel, f)
		}
	}
}
