// Package initd is what herd-guest does as PID 1 of the microVM, in order: mount the guest's pseudo filesystems,
// mount the virtio-fs share, create every DurableDir volume as a subdirectory of the share, then — only after all
// volumes exist — write each container's OCI bundle and start it. The plan (which containers, which volumes) is
// written by herdlet from the ActorTemplate into the share at .herd/actor.json.
package initd

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// PlanPath is the plan's path relative to the share.
const PlanPath = ".herd/actor.json"

// Plan mirrors the parts of a Substrate ActorTemplate the guest acts on, with images already resolved to block
// devices by herdlet.
type Plan struct {
	Volumes    []Volume    `json:"volumes"`
	Containers []Container `json:"containers"`
}

// Volume is a DurableDir volume: a directory of the share named after the volume.
type Volume struct {
	Name string `json:"name"`
}

// Container is one OCI container of the template.
type Container struct {
	Name string `json:"name"`
	// Image is the container's read-only root image (erofs or ext4) as a block device; a writable layer goes on top.
	Image        Image         `json:"image"`
	Command      []string      `json:"command"`
	Args         []string      `json:"args,omitempty"`
	Env          []EnvVar      `json:"env,omitempty"`
	Workdir      string        `json:"workdir,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
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
	return nil
}
