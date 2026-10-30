// Package oci runs the template's containers. Runtime is the boundary; Crun drives the crun binary in the guest
// initramfs. Spec writes the OCI runtime spec (config.json) of one container: its rootfs, the guest's pseudo
// filesystems, its DurableDir volumes as bind mounts of subdirectories of the virtio-fs share, the guest files it is
// given (resolv.conf, trust bundles with herd-egress's CA) as read-only binds, and herd-guest itself
// at /.herd/herd-guest (read-only) so the container's init, the exec shim and the signal helper run inside it.
package oci

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// GuestBinary is where herd-guest is bound inside every container.
const GuestBinary = "/.herd/herd-guest"

// The subset of the OCI runtime spec (v1.2) herd-guest writes.
type (
	Spec struct {
		Version  string  `json:"ociVersion"`
		Process  Process `json:"process"`
		Root     Root    `json:"root"`
		Hostname string  `json:"hostname,omitempty"`
		Mounts   []Mount `json:"mounts"`
		Linux    Linux   `json:"linux"`
	}
	Process struct {
		Terminal        bool     `json:"terminal"`
		User            User     `json:"user"`
		Args            []string `json:"args"`
		Env             []string `json:"env"`
		Cwd             string   `json:"cwd"`
		NoNewPrivileges bool     `json:"noNewPrivileges"`
	}
	User struct {
		UID uint32 `json:"uid"`
		GID uint32 `json:"gid"`
	}
	Root struct {
		Path     string `json:"path"`
		Readonly bool   `json:"readonly"`
	}
	Mount struct {
		Destination string   `json:"destination"`
		Type        string   `json:"type,omitempty"`
		Source      string   `json:"source,omitempty"`
		Options     []string `json:"options,omitempty"`
	}
	Linux struct {
		Namespaces    []Namespace `json:"namespaces"`
		MaskedPaths   []string    `json:"maskedPaths,omitempty"`
		ReadonlyPaths []string    `json:"readonlyPaths,omitempty"`
	}
	Namespace struct {
		Type string `json:"type"`
	}
)

// VolumeMount is one DurableDir volume bound into a container. Source must already be a directory inside the
// share; the caller (initd) guarantees it, and Build refuses a source outside ShareDir.
type VolumeMount struct {
	Source      string
	Destination string
	ReadOnly    bool
}

// FileMount is one guest file bound read-only (nosuid, nodev) at Destination in the container; both paths are clean
// and absolute.
type FileMount struct {
	Source      string
	Destination string
}

// Config is what Build needs for one container.
type Config struct {
	Name     string
	Rootfs   string
	Args     []string
	Env      []string
	Cwd      string
	ShareDir string
	Volumes  []VolumeMount
	// Files are guest files bound read-only into the container (its resolv.conf and trust bundles).
	Files []FileMount
	// GuestBinary is herd-guest's path in the guest, bound at GuestBinary in the container.
	GuestBinary string
}

// cleanAbs reports whether p is absolute, clean and not "/".
func cleanAbs(p string) bool { return path.IsAbs(p) && path.Clean(p) == p && p != "/" }

// within reports whether p is dir or below it (both clean and absolute).
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// Build returns the container's runtime spec. The container's process is herd-guest's container init, which runs
// Args and reaps the shims that exec leaves behind.
func Build(c Config) (*Spec, error) {
	if len(c.Args) == 0 {
		return nil, fmt.Errorf("oci: container %q has no command", c.Name)
	}
	if !path.IsAbs(c.ShareDir) || path.Clean(c.ShareDir) != c.ShareDir || c.ShareDir == "/" {
		return nil, fmt.Errorf("oci: share %q is not a clean absolute directory", c.ShareDir)
	}
	cwd := c.Cwd
	if cwd == "" {
		cwd = "/"
	}
	env := c.Env
	if !hasEnv(env, "PATH") {
		env = append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, env...)
	}
	s := &Spec{
		Version: "1.2.0",
		Process: Process{
			Args:            append([]string{GuestBinary, "cinit", "--"}, c.Args...),
			Env:             env,
			Cwd:             cwd,
			NoNewPrivileges: true,
		},
		Root:     Root{Path: c.Rootfs},
		Hostname: c.Name,
		Mounts: []Mount{
			{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/sys/fs/cgroup", Type: "cgroup2", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
			{Destination: GuestBinary, Type: "bind", Source: c.GuestBinary, Options: []string{"rbind", "ro", "nosuid", "nodev"}},
		},
		Linux: Linux{
			Namespaces: []Namespace{{"pid"}, {"ipc"}, {"uts"}, {"mount"}, {"cgroup"}},
			MaskedPaths: []string{"/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list",
				"/proc/timer_stats", "/proc/sched_debug", "/proc/scsi", "/sys/firmware"},
			ReadonlyPaths: []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"},
		},
	}
	seen := map[string]bool{}
	for _, v := range c.Volumes {
		src := path.Clean(v.Source)
		if src != v.Source || !within(src, c.ShareDir) || src == c.ShareDir {
			return nil, fmt.Errorf("oci: container %q: volume source %q is not a directory inside the share %s", c.Name, v.Source, c.ShareDir)
		}
		if seen[v.Destination] {
			return nil, fmt.Errorf("oci: container %q: two mounts at %s", c.Name, v.Destination)
		}
		seen[v.Destination] = true
		opts := []string{"rbind", "nosuid", "nodev"}
		if v.ReadOnly {
			opts = append(opts, "ro")
		}
		s.Mounts = append(s.Mounts, Mount{Destination: v.Destination, Type: "bind", Source: src, Options: opts})
	}
	for _, f := range c.Files {
		if !cleanAbs(f.Source) || !cleanAbs(f.Destination) || within(f.Destination, "/.herd") {
			return nil, fmt.Errorf("oci: container %q: file mount %q at %q needs clean absolute paths", c.Name, f.Source, f.Destination)
		}
		if seen[f.Destination] {
			return nil, fmt.Errorf("oci: container %q: two mounts at %s", c.Name, f.Destination)
		}
		seen[f.Destination] = true
		s.Mounts = append(s.Mounts, Mount{Destination: f.Destination, Type: "bind", Source: f.Source, Options: []string{"bind", "ro", "nosuid", "nodev"}})
	}
	return s, nil
}

func hasEnv(env []string, k string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, k+"=") {
			return true
		}
	}
	return false
}

// Marshal renders the spec as config.json.
func (s *Spec) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
