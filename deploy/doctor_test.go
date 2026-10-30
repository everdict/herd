package deploy_test

import (
	"errors"
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
