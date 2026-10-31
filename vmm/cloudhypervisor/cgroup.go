package cloudhypervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Cgroups gives each VM a cgroup of its own that its cloud-hypervisor and virtiofsd processes start in, so that a
// VM's memory (its guest memory is shared memory, charged in full) is charged to the VM and never to the herdlet's
// own cgroup: a Nomad task's memory limit or a systemd unit's MemoryMax is the herdlet's, not its VMs' (PLT-214).
type Cgroups interface {
	// Create makes the VM's cgroup (or takes the one a crashed herdlet left), limited to memoryMax bytes (0: no
	// limit), and returns its directory, which ProcSpec.Cgroup names.
	Create(vm string, memoryMax int64) (string, error)
	// Remove removes the VM's cgroup once its processes have exited; an absent one counts as removed.
	Remove(vm string) error
}

// DefaultMemoryOverheadMiB is a VM cgroup's room beyond its guest memory (Config.MemoryOverheadMiB).
const DefaultMemoryOverheadMiB = 512

// DefaultCgroupRoot is where the host mounts the cgroup v2 hierarchy.
const DefaultCgroupRoot = "/sys/fs/cgroup"

// DefaultCgroupParent is herd's cgroup under the root: VM cgroups are <root>/herd/<vm>, outside every Nomad task
// scope and systemd unit.
const DefaultCgroupParent = "herd"

// CgroupV2 places VMs in a cgroup v2 hierarchy: <Root>/<Parent>/<vm>, with the memory controller enabled so that
// each VM's memory.current accounts for it and memory.max limits it. It needs a writable hierarchy (herdlet as root).
type CgroupV2 struct {
	// Root is the cgroup v2 mount; empty means DefaultCgroupRoot.
	Root string
	// Parent is herd's cgroup, relative to Root; empty means DefaultCgroupParent.
	Parent string

	rmdir func(string) error // nil: syscall.Rmdir (a cgroupfs removes a cgroup's control files with it)
}

var _ Cgroups = CgroupV2{}

// cgroupNameRE is a VM id (idRE): one cgroup directory name.
var cgroupNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (c CgroupV2) root() string {
	if c.Root == "" {
		return DefaultCgroupRoot
	}
	return c.Root
}

func (c CgroupV2) parent() string {
	if c.Parent == "" {
		return DefaultCgroupParent
	}
	return filepath.Clean(c.Parent)
}

// Dir is the VM's cgroup directory.
func (c CgroupV2) Dir(vm string) string { return filepath.Join(c.root(), c.parent(), vm) }

// prepare checks the hierarchy is cgroup v2 with the memory controller, and makes herd's cgroup with the memory
// controller enabled for its children.
func (c CgroupV2) prepare() error {
	ctrl, err := os.ReadFile(filepath.Join(c.root(), "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup: %s is not a cgroup v2 hierarchy (%v): herd places each VM in a cgroup of its own and needs cgroup v2", c.root(), err)
	}
	if !hasWord(string(ctrl), "memory") {
		return fmt.Errorf("cgroup: %s does not offer the memory controller (it offers %q)", c.root(), strings.TrimSpace(string(ctrl)))
	}
	// Every level from the root down to herd's cgroup enables memory for its children; the root's own processes do
	// not stop it (the root is exempt from the no-internal-processes rule), and herd's cgroup holds no process.
	dir := c.root()
	for _, part := range strings.Split(c.parent(), string(filepath.Separator)) {
		if err := enable(dir, "memory"); err != nil {
			return err
		}
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("cgroup: %w", err)
		}
	}
	return enable(dir, "memory")
}

// enable turns a controller on for dir's children, unless it is on already.
func enable(dir, controller string) error {
	f := filepath.Join(dir, "cgroup.subtree_control")
	if cur, err := os.ReadFile(f); err == nil && hasWord(string(cur), controller) {
		return nil
	}
	if err := os.WriteFile(f, []byte("+"+controller), 0o644); err != nil {
		return fmt.Errorf("cgroup: enable %s in %s: %w", controller, dir, err)
	}
	return nil
}

func hasWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}

func (c CgroupV2) Create(vm string, memoryMax int64) (string, error) {
	if !cgroupNameRE.MatchString(vm) {
		return "", fmt.Errorf("cgroup: %q is not a VM id", vm)
	}
	if err := c.prepare(); err != nil {
		return "", err
	}
	dir := c.Dir(vm)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("cgroup: %w", err)
	}
	limit := "max"
	if memoryMax > 0 {
		limit = strconv.FormatInt(memoryMax, 10)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(limit), 0o644); err != nil {
		_ = c.Remove(vm)
		return "", fmt.Errorf("cgroup: limit %s: %w", vm, err)
	}
	return dir, nil
}

func (c CgroupV2) Remove(vm string) error {
	if !cgroupNameRE.MatchString(vm) {
		return fmt.Errorf("cgroup: %q is not a VM id", vm)
	}
	rmdir := c.rmdir
	if rmdir == nil {
		rmdir = syscall.Rmdir
	}
	dir := c.Dir(vm)
	// A process observed exited may still be leaving the cgroup for a moment: rmdir answers EBUSY until it has.
	var err error
	for i := 0; i < 50; i++ {
		err = rmdir(dir)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("cgroup: remove %s: %w", dir, err)
}

// Check verifies VMs can be placed here: a writable cgroup v2 hierarchy with the memory controller, in which herd's
// cgroup is not inside self, the cgroup of the process that will start the VMs (as /proc/self/cgroup names it,
// SelfCgroup). It makes and removes a probe cgroup.
func (c CgroupV2) Check(self string) error {
	if self != "" && self != "/" {
		herd := "/" + filepath.ToSlash(c.parent())
		if herd == self || strings.HasPrefix(herd, strings.TrimSuffix(self, "/")+"/") {
			return fmt.Errorf("cgroup: herd's cgroup %s is inside this process's own cgroup %s, so its VMs would be charged to it", herd, self)
		}
	}
	probe := "doctor-" + strconv.Itoa(os.Getpid())
	if _, err := c.Create(probe, 64<<20); err != nil {
		return err
	}
	return c.Remove(probe)
}

// SelfCgroup is this process's cgroup v2 path, from /proc/self/cgroup ("0::/nomad.slice/...").
func SelfCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup: %w", err)
	}
	return ParseProcCgroup(string(data))
}

// ParseProcCgroup reads the cgroup v2 path out of a /proc/<pid>/cgroup file.
func ParseProcCgroup(data string) (string, error) {
	for _, l := range strings.Split(data, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(l), "0::"); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("cgroup: no cgroup v2 entry in %q", strings.TrimSpace(data))
}
