package initd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/guest/oci"
)

// Mounter is the mount(2) boundary; the linux implementation calls the kernel, tests record.
type Mounter interface {
	Mount(source, target, fstype string, flags uintptr, data string) error
}

// Layout is where things go in the guest.
type Layout struct {
	// ShareDir is where the virtio-fs share is mounted (/mnt/volumes in the initramfs).
	ShareDir string
	// RunDir holds images, writable layers and bundles (/run/herd, on tmpfs).
	RunDir string
	// GuestBinary is herd-guest's own path (/init).
	GuestBinary string
}

// DefaultLayout is the initramfs's layout.
var DefaultLayout = Layout{ShareDir: "/mnt/volumes", RunDir: "/run/herd", GuestBinary: "/init"}

// Booted is a started container.
type Booted struct {
	Name    string
	Rootfs  string
	Workdir string
	Bundle  string
	// Volumes maps each volume's mount path in the container to its directory in the share.
	Volumes map[string]string
}

// Pseudo mounts the guest's own pseudo filesystems.
func Pseudo(m Mounter) error {
	for _, p := range []struct{ src, dst, typ, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", ""},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", ""},
	} {
		if err := os.MkdirAll(p.dst, 0o755); err != nil {
			return fmt.Errorf("initd: %w", err)
		}
		if err := m.Mount(p.src, p.dst, p.typ, 0, p.data); err != nil {
			return fmt.Errorf("initd: mount %s on %s: %w", p.typ, p.dst, err)
		}
	}
	return nil
}

// MountShare mounts the virtio-fs share and reads the plan from it.
func MountShare(m Mounter, l Layout, tag string) error {
	if err := os.MkdirAll(l.ShareDir, 0o755); err != nil {
		return fmt.Errorf("initd: %w", err)
	}
	if err := m.Mount(tag, l.ShareDir, "virtiofs", 0, ""); err != nil {
		return fmt.Errorf("initd: mount virtio-fs %q on %s: %w", tag, l.ShareDir, err)
	}
	return nil
}

// ReadPlan reads the plan herdlet left in the mounted share.
func ReadPlan(l Layout) (*Plan, error) {
	root, err := os.OpenRoot(l.ShareDir)
	if err != nil {
		return nil, fmt.Errorf("initd: %w", err)
	}
	defer root.Close()
	b, err := root.ReadFile(PlanPath)
	if err != nil {
		return nil, fmt.Errorf("initd: reading the plan: %w", err)
	}
	return ParsePlan(b)
}

// Volumes creates each volume as a directory of the share (keeping one that a resumed actor already has) and
// returns their guest paths. It goes through os.Root, and refuses a name the share already holds as anything but a
// directory: a symlink there would bind something outside the share into the container.
func Volumes(l Layout, p *Plan) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(l.ShareDir)
	if err != nil {
		return nil, fmt.Errorf("initd: %w", err)
	}
	defer root.Close()
	out := map[string]string{}
	for _, v := range p.Volumes {
		if err := root.Mkdir(v.Name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("initd: volume %q: %w", v.Name, err)
		}
		fi, err := root.Lstat(v.Name)
		if err != nil {
			return nil, fmt.Errorf("initd: volume %q: %w", v.Name, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("initd: volume %q: %s in the share is a %v, not a directory", v.Name, v.Name, fi.Mode().Type())
		}
		out[v.Name] = filepath.Join(l.ShareDir, v.Name)
	}
	return out, nil
}

// Containers prepares and starts every container. vols must come from Volumes: every volume a container mounts
// exists before the first container starts.
func Containers(ctx context.Context, m Mounter, rt oci.Runtime, l Layout, p *Plan, vols map[string]string) ([]Booted, error) {
	for _, c := range p.Containers {
		for _, vm := range c.VolumeMounts {
			if _, ok := vols[vm.Name]; !ok {
				return nil, fmt.Errorf("initd: container %q: volume %q was not created", c.Name, vm.Name)
			}
		}
	}
	var booted []Booted
	for _, c := range p.Containers {
		b, err := prepare(m, l, c, vols)
		if err != nil {
			return booted, err
		}
		if err := rt.Run(ctx, c.Name, b.Bundle); err != nil {
			return booted, fmt.Errorf("initd: starting container %q: %w", c.Name, err)
		}
		booted = append(booted, b)
	}
	return booted, nil
}

// prepare mounts the container's image read-only, puts a writable overlay on top, and writes its config.json.
func prepare(m Mounter, l Layout, c Container, vols map[string]string) (Booted, error) {
	dir := filepath.Join(l.RunDir, "containers", c.Name)
	lower, upper, work := filepath.Join(dir, "image"), filepath.Join(dir, "upper"), filepath.Join(dir, "work")
	rootfs := filepath.Join(dir, "rootfs")
	for _, d := range []string{lower, upper, work, rootfs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Booted{}, fmt.Errorf("initd: %w", err)
		}
	}
	const msRdonly = 0x1
	if err := m.Mount(c.Image.Device, lower, c.Image.FSType, msRdonly, ""); err != nil {
		return Booted{}, fmt.Errorf("initd: container %q: mount image %s: %w", c.Name, c.Image.Device, err)
	}
	if err := m.Mount("overlay", rootfs, "overlay", 0, fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)); err != nil {
		return Booted{}, fmt.Errorf("initd: container %q: overlay: %w", c.Name, err)
	}
	for _, vm := range c.VolumeMounts {
		if err := bindVolume(m, rootfs, vols[vm.Name], vm); err != nil {
			return Booted{}, fmt.Errorf("initd: container %q: volume %q at %s: %w", c.Name, vm.Name, vm.MountPath, err)
		}
	}
	cfg := oci.Config{
		Name: c.Name, Rootfs: rootfs, Args: append(append([]string(nil), c.Command...), c.Args...),
		Cwd: c.Workdir, ShareDir: l.ShareDir, GuestBinary: l.GuestBinary,
	}
	for _, e := range c.Env {
		cfg.Env = append(cfg.Env, e.Name+"="+e.Value)
	}
	mounted := map[string]string{}
	for _, vm := range c.VolumeMounts {
		cfg.Volumes = append(cfg.Volumes, oci.VolumeMount{Source: vols[vm.Name], Destination: vm.MountPath, ReadOnly: vm.ReadOnly})
		mounted[vm.MountPath] = vols[vm.Name]
	}
	spec, err := oci.Build(cfg)
	if err != nil {
		return Booted{}, err
	}
	b, err := spec.Marshal()
	if err != nil {
		return Booted{}, fmt.Errorf("initd: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		return Booted{}, fmt.Errorf("initd: %w", err)
	}
	return Booted{Name: c.Name, Rootfs: rootfs, Workdir: c.Workdir, Bundle: dir, Volumes: mounted}, nil
}

// bindVolume binds a volume's share directory onto the guest's own view of the container's rootfs, which is where
// herd-guest's file API resolves paths (PLT-195). crun's bind of the volume exists only in the container's mount
// namespace: without this one, a file the API writes under the volume's path lands in the overlay on /run and is lost
// with the VM, while the container's processes write to the share. The mount point is made and opened through
// os.Root, so a symlink in the image cannot place it outside the rootfs, and mounted on as /proc/self/fd/<n>.
func bindVolume(m Mounter, rootfs, source string, v VolumeMount) error {
	const msRdonly, msRemount, msBind = 0x1, 0x20, 0x1000
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := strings.TrimPrefix(path.Clean(v.MountPath), "/")
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if err := mountOn(root, rel, func(target string) error { return m.Mount(source, target, "", msBind, "") }); err != nil {
		return err
	}
	if !v.ReadOnly {
		return nil
	}
	// Opened again, the path now resolves to the bind mount's root, which is what the remount must name.
	return mountOn(root, rel, func(target string) error { return m.Mount("", target, "", msRemount|msBind|msRdonly, "") })
}

func mountOn(root *os.Root, rel string, mount func(target string) error) error {
	f, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	return mount(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}

// Boot runs the whole sequence after the pseudo filesystems: share, plan, volumes, containers.
func Boot(ctx context.Context, m Mounter, rt oci.Runtime, l Layout, shareTag string) (*Plan, []Booted, error) {
	if err := MountShare(m, l, shareTag); err != nil {
		return nil, nil, err
	}
	p, err := ReadPlan(l)
	if err != nil {
		return nil, nil, err
	}
	vols, err := Volumes(l, p)
	if err != nil {
		return p, nil, err
	}
	booted, err := Containers(ctx, m, rt, l, p, vols)
	return p, booted, err
}

// Target is the started container as herd-guest's API serves it, with its volumes where the container sees them.
func (b Booted) Target(rt oci.Runtime) *oci.Target {
	return &oci.Target{ContainerName: b.Name, RootfsDir: b.Rootfs, Dir: b.Workdir, Runtime: rt, Volumes: b.Volumes}
}
