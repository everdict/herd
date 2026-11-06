package machine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/link"
)

// ShareTag is the virtio-fs tag of the VM's share; herd-guest mounts it at /mnt/volumes (ADR A-10).
const ShareTag = "herd"

// device is the guest block device of the i-th rootfs image: the hypervisors attach them as virtio-blk disks in the
// order of vmm.Spec.Rootfs, and the guest names them vda, vdb, ...
func device(i int) (string, error) {
	if i >= 26 {
		return "", fmt.Errorf("machine: more than 26 rootfs images")
	}
	return "/dev/vd" + string(rune('a'+i)), nil
}

// argv applies Substrate's rules: command replaces ENTRYPOINT and CMD, args alone replace CMD.
func argv(c link.Container, cfg image.Config) []string {
	switch {
	case len(c.Command) > 0:
		return append(append([]string(nil), c.Command...), c.Args...)
	case len(c.Args) > 0:
		return append(append([]string(nil), cfg.Entrypoint...), c.Args...)
	}
	return append(append([]string(nil), cfg.Entrypoint...), cfg.Cmd...)
}

// env is the image's environment overridden by the template's, by name.
func env(c link.Container, cfg image.Config) []initd.EnvVar {
	var out []initd.EnvVar
	at := map[string]int{}
	set := func(name, value string) {
		if i, ok := at[name]; ok {
			out[i].Value = value
			return
		}
		at[name] = len(out)
		out = append(out, initd.EnvVar{Name: name, Value: value})
	}
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for _, e := range c.Env {
		set(e.Name, e.Value)
	}
	return out
}

// Plan turns a template whose images are built into herd-guest's plan and the VM's rootfs list: one block device per
// distinct image, in order.
func Plan(t link.Template, roots map[string]image.Rootfs) (*initd.Plan, []string, error) {
	p := &initd.Plan{}
	for _, v := range t.Volumes {
		p.Volumes = append(p.Volumes, initd.Volume{Name: v})
	}
	var disks []string
	devs := map[string]string{}
	for _, c := range t.Containers {
		r, ok := roots[c.Image]
		if !ok {
			return nil, nil, fmt.Errorf("machine: container %q: image %s not built", c.Name, c.Image)
		}
		dev, ok := devs[r.Path]
		if !ok {
			var err error
			if dev, err = device(len(disks)); err != nil {
				return nil, nil, err
			}
			devs[r.Path] = dev
			disks = append(disks, r.Path)
		}
		ic := initd.Container{Name: c.Name, Image: initd.Image{Device: dev, FSType: r.FSType}, Command: argv(c, r.Config), Env: env(c, r.Config),
			User: r.Config.User}
		// Docker runs in the image's WORKDIR, cleaned (a trailing slash is still the directory).
		if wd := path.Clean(r.Config.WorkingDir); r.Config.WorkingDir != "" && wd != "/" && strings.HasPrefix(wd, "/") {
			ic.Workdir = wd
		}
		for _, m := range c.VolumeMounts {
			ic.VolumeMounts = append(ic.VolumeMounts, initd.VolumeMount{Name: m.Name, MountPath: m.MountPath})
		}
		p.Containers = append(p.Containers, ic)
	}
	if err := p.Validate(); err != nil {
		return nil, nil, fmt.Errorf("machine: %w", err)
	}
	return p, disks, nil
}

// ErrOutsideShare is answered for a volume path that would leave the VM's share.
var ErrOutsideShare = errors.New("machine: path leaves the share")

// layVolumes creates every DurableDir volume as a directory of the share, through root so nothing can lead outside
// it: a name must be one path element (a DNS label, checked by Plan), and an existing entry under that name must be a
// directory, not a symlink.
func layVolumes(root *os.Root, p *initd.Plan) error {
	for _, v := range p.Volumes {
		if err := root.Mkdir(v.Name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("machine: volume %q: %w", v.Name, err)
		}
		fi, err := root.Lstat(v.Name)
		if err != nil {
			return fmt.Errorf("machine: volume %q: %w", v.Name, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: volume %q is a %v in the share, not a directory", ErrOutsideShare, v.Name, fi.Mode().Type())
		}
		// Shared like a Kubernetes emptyDir: an image may run as any user, and the umask must not decide that.
		if err := root.Chmod(v.Name, 0o777); err != nil {
			return fmt.Errorf("machine: volume %q: %w", v.Name, err)
		}
	}
	return nil
}

// writePlan writes the plan where herd-guest reads it (initd.PlanPath).
func writePlan(root *os.Root, p *initd.Plan) error {
	b, err := marshalPlan(p)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(path.Dir(initd.PlanPath), 0o755); err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	if err := root.WriteFile(initd.PlanPath, b, 0o644); err != nil {
		return fmt.Errorf("machine: plan: %w", err)
	}
	return nil
}
