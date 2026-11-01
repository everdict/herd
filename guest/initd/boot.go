package initd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"time"

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
	// SysClassNet is sysfs's list of network interfaces (SysClassNet), where the plan's NIC is found by MAC.
	SysClassNet string
	// ProbeDNS, if set, is asked once the network is up for a name through the plan's first resolver; its answer is
	// logged, never fatal (DefaultLayout: the real query, so the console shows whether herd-egress's DNS answers).
	ProbeDNS func(server, name string, d time.Duration) (netip.Addr, error)
}

// DefaultLayout is the initramfs's layout.
var DefaultLayout = Layout{ShareDir: "/mnt/volumes", RunDir: "/run/herd", GuestBinary: "/init", SysClassNet: SysClassNet, ProbeDNS: ProbeDNS}

// Booted is a started container.
type Booted struct {
	Name    string
	Rootfs  string
	Workdir string
	Bundle  string
	// Volumes maps each volume's mount path in the container to its directory in the share.
	Volumes map[string]string
	// User is the identity the container's process runs as (the image's USER, resolved in its rootfs).
	User oci.User
	// Watch follows the container's main process (nil if the runtime gave none).
	Watch *oci.Watch
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
		b, err := prepare(m, l, c, vols, p.Network)
		if err != nil {
			return booted, err
		}
		w, err := rt.Run(ctx, c.Name, b.Bundle)
		if err != nil {
			return booted, fmt.Errorf("initd: starting container %q: %w", c.Name, err)
		}
		b.Watch = w
		booted = append(booted, b)
	}
	return booted, nil
}

// prepare mounts the container's image read-only, puts a writable overlay on top, and writes its config.json.
// With a network, the container also gets the guest's resolver and herd-egress's CA in its trust bundles (netFiles).
func prepare(m Mounter, l Layout, c Container, vols map[string]string, n *Network) (Booted, error) {
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
	user, err := oci.ResolveUser(rootfs, c.User)
	if err != nil {
		return Booted{}, fmt.Errorf("initd: container %q: %w", c.Name, err)
	}
	cfg := oci.Config{
		Name: c.Name, Rootfs: rootfs, Args: append(append([]string(nil), c.Command...), c.Args...),
		Cwd: c.Workdir, User: user, ShareDir: l.ShareDir, GuestBinary: l.GuestBinary,
	}
	for _, e := range c.Env {
		cfg.Env = append(cfg.Env, e.Name+"="+e.Value)
	}
	mounted := c.Mounts(vols)
	for _, vm := range c.VolumeMounts {
		cfg.Volumes = append(cfg.Volumes, oci.VolumeMount{Source: mounted[vm.MountPath], Destination: vm.MountPath, ReadOnly: vm.ReadOnly})
	}
	files, err := netFiles(l, n, rootfs, dir)
	if err != nil {
		return Booted{}, fmt.Errorf("initd: container %q: %w", c.Name, err)
	}
	cfg.Files = files
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
	return Booted{Name: c.Name, Rootfs: rootfs, Workdir: c.Workdir, Bundle: dir, Volumes: mounted, User: user}, nil
}

// Mounts maps each of c's volume mount paths to the volume's directory in vols (from Volumes): every container that
// mounts a volume binds the same directory, so they share it.
func (c Container) Mounts(vols map[string]string) map[string]string {
	out := map[string]string{}
	for _, vm := range c.VolumeMounts {
		out[vm.MountPath] = vols[vm.Name]
	}
	return out
}

// Boot runs the whole sequence after the pseudo filesystems: share, plan, network (if the plan has one, through nl,
// with the guest files the containers are given), volumes, containers.
func Boot(ctx context.Context, m Mounter, nl Netlinker, rt oci.Runtime, l Layout, shareTag string) (*Plan, []Booted, error) {
	if err := MountShare(m, l, shareTag); err != nil {
		return nil, nil, err
	}
	// A share kept from an earlier boot may hold its exits; they are not this boot's.
	if root, err := os.OpenRoot(l.ShareDir); err == nil {
		_ = root.Remove(ExitPath)
		root.Close()
	}
	p, err := ReadPlan(l)
	if err != nil {
		return nil, nil, err
	}
	if p.Network != nil {
		sys := l.SysClassNet
		if sys == "" {
			sys = SysClassNet
		}
		if err := ConfigureNetwork(nl, sys, p.Network); err != nil {
			return p, nil, err
		}
		if err := WriteNetFiles(l, p.Network); err != nil {
			return p, nil, err
		}
		if l.ProbeDNS != nil && len(p.Network.DNS) > 0 {
			start := time.Now()
			if a, err := l.ProbeDNS(p.Network.DNS[0], "herd-guest.probe", 2*time.Second); err != nil {
				log.Printf("herd-guest: network: DNS probe via %s: %v: names will not resolve in the containers", p.Network.DNS[0], err)
			} else {
				log.Printf("herd-guest: network: DNS probe via %s answered %s in %v", p.Network.DNS[0], a, time.Since(start).Round(time.Millisecond))
			}
		}
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
	return &oci.Target{ContainerName: b.Name, RootfsDir: b.Rootfs, Dir: b.Workdir, Runtime: rt, Volumes: b.Volumes, User: b.User, Watch: b.Watch}
}

// BootErrorPath is where herd-guest leaves, relative to the share, why its boot failed, before it ends the VM: the
// host reads it (machine.Machines) and fails the boot with the reason at once, instead of waiting for a guest that
// will never answer.
const BootErrorPath = ".herd/boot-error"

// WriteBootError records err at BootErrorPath in the share, synced, so the host sees it once the VM has ended.
func WriteBootError(l Layout, err error) error {
	root, rerr := os.OpenRoot(l.ShareDir)
	if rerr != nil {
		return fmt.Errorf("initd: %w", rerr)
	}
	defer root.Close()
	if rerr := root.MkdirAll(filepath.Dir(BootErrorPath), 0o755); rerr != nil {
		return fmt.Errorf("initd: %w", rerr)
	}
	f, rerr := root.OpenFile(BootErrorPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if rerr != nil {
		return fmt.Errorf("initd: %w", rerr)
	}
	_, rerr = f.WriteString(err.Error() + "\n")
	if rerr == nil {
		rerr = f.Sync()
	}
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	return rerr
}

// ExitPath is where herd-guest records, relative to the share, each container whose main process ended while the VM
// runs (oci.ContainerExit: its status and last output). The host reads it (machine.Machines.ContainerExit) and reports
// the actor CRASHED with it; the VM itself keeps running until the actor is deleted.
const ExitPath = ".herd/container-exit"

// ReportExit waits until b's main process has ended, then logs it to the console and records it at ExitPath. It
// returns at once for a container with no Watch, and when ctx ends.
func ReportExit(ctx context.Context, l Layout, b Booted) error {
	if b.Watch == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.Watch.Done():
	}
	e := b.Watch.Exited()
	if e == nil {
		return nil
	}
	log.Printf("herd-guest: %v", e)
	return appendShareFile(l, ExitPath, e.Error()+"\n")
}

// appendShareFile appends s to name in the share, synced, through os.Root.
func appendShareFile(l Layout, name, s string) error {
	root, err := os.OpenRoot(l.ShareDir)
	if err != nil {
		return fmt.Errorf("initd: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return fmt.Errorf("initd: %w", err)
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("initd: %w", err)
	}
	_, err = f.WriteString(s)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
