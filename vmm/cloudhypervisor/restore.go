//go:build linux

package cloudhypervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/everdict/herd/vmm"
)

// restoreSource lays out what Cloud Hypervisor restores the VM spec from: the snapshot in dir, with its config.json
// rewritten to name this VM's host paths (rewriteConfig) and every other file linked. It lives in the VM's own
// directory, so the published snapshot is never changed and goes with the VM.
func (v *VMM) restoreSource(dir string, spec vmm.Spec, vmDir string) (string, error) {
	src := filepath.Join(vmDir, "restore")
	if err := os.Mkdir(src, 0o700); err != nil {
		return "", fmt.Errorf("cloud-hypervisor: restore %s: %w", spec.ID, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor: restore %s: %w", spec.ID, err)
	}
	for _, e := range ents {
		if e.Name() == "config.json" {
			continue
		}
		if err := os.Symlink(filepath.Join(dir, e.Name()), filepath.Join(src, e.Name())); err != nil {
			return "", fmt.Errorf("cloud-hypervisor: restore %s: %w", spec.ID, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor: restore %s: snapshot config: %w", spec.ID, err)
	}
	cfg, err := rewriteConfig(data, v.vmConfig(spec, vmDir))
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor: restore %s: %w", spec.ID, err)
	}
	if err := writeFileSync(filepath.Join(src, "config.json"), cfg); err != nil {
		return "", fmt.Errorf("cloud-hypervisor: restore %s: %w", spec.ID, err)
	}
	return src, nil
}

// rewriteConfig sets, in the VM config Cloud Hypervisor saved with a snapshot, every host path and host device to
// the restoring VM's (want): the kernel and initramfs, each disk's image (by position: the device order is the
// guest's), each virtio-fs socket (by tag), the vsock socket, the serial and console files, and each NIC's TAP and
// MAC. Everything else is kept as saved, including the fields herd does not know. A snapshot whose devices are not
// the ones want has is refused with vmm.ErrInvalidSpec: its device state could not be restored onto them.
func rewriteConfig(data []byte, want vmConfig) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var c map[string]any
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("snapshot config: %w", err)
	}
	mismatch := func(what string, got, wantN int) error {
		return fmt.Errorf("%w: the snapshot has %d %s, the VM it restores into %d", vmm.ErrInvalidSpec, got, what, wantN)
	}
	if p, ok := c["payload"].(map[string]any); ok {
		p["kernel"] = want.Payload.Kernel
		if want.Payload.Initramfs != "" {
			p["initramfs"] = want.Payload.Initramfs
		} else {
			delete(p, "initramfs")
		}
	}
	disks, _ := c["disks"].([]any)
	if len(disks) != len(want.Disks) {
		return nil, mismatch("disks", len(disks), len(want.Disks))
	}
	for i, x := range disks {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("snapshot config: disk %d is %T", i, x)
		}
		m["path"] = want.Disks[i].Path
	}
	fsList, _ := c["fs"].([]any)
	if len(fsList) != len(want.Fs) {
		return nil, mismatch("virtio-fs shares", len(fsList), len(want.Fs))
	}
	for i, x := range fsList {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("snapshot config: fs %d is %T", i, x)
		}
		found := false
		for _, f := range want.Fs {
			if m["tag"] == f.Tag {
				m["socket"], found = f.Socket, true
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: the snapshot has a share %v the VM it restores into has not", vmm.ErrInvalidSpec, m["tag"])
		}
	}
	nets, _ := c["net"].([]any)
	if len(nets) != len(want.Net) {
		return nil, mismatch("NICs", len(nets), len(want.Net))
	}
	for i, x := range nets {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("snapshot config: net %d is %T", i, x)
		}
		m["tap"], m["mac"] = want.Net[i].Tap, want.Net[i].MAC
	}
	if m, ok := c["vsock"].(map[string]any); ok && want.Vsock != nil {
		m["socket"] = want.Vsock.Socket
	}
	for key, w := range map[string]*consoleConfig{"serial": want.Serial, "console": want.Console} {
		if m, ok := c[key].(map[string]any); ok && w != nil && m["file"] != nil {
			m["file"] = w.File
		}
	}
	return json.Marshal(c)
}

// evidenceLines is how many lines of each log a failed start keeps.
const evidenceLines = 40

// readEvidence reads the tail of every log in a VM's directory: what cloud-hypervisor and virtiofsd wrote to stdout and
// stderr, and the guest's serial console. It runs before a failed VM's directory is removed.
func readEvidence(dir string) map[string]string {
	logs := map[string]string{}
	names, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for _, p := range names {
		if tail := logLines(p, evidenceLines); tail != "" {
			logs[filepath.Base(p)] = tail
		}
	}
	return logs
}

// report logs the evidence of a VM that failed to start, before its directory goes: how cloud-hypervisor ended (or
// that it still ran), each log's tail (cloud-hypervisor's stdout and stderr, with its info-level steps, virtiofsd's,
// the serial console), an empty or missing cloud-hypervisor log said as such, and what the kernel logged about the
// process (an OOM kill, a segfault, a seccomp audit record). It answers a short account for the error.
func (v *VMM) report(id string, in *instance, logs map[string]string) string {
	var short []string
	if in.ch != nil {
		status := "still running when the start failed"
		if s, ok := in.ch.(interface{ ExitStatus() string }); ok && s.ExitStatus() != "" {
			status = "ended: " + s.ExitStatus()
		}
		line := fmt.Sprintf("cloud-hypervisor (pid %d) %s", in.ch.Pid(), status)
		log.Printf("cloud-hypervisor: %s failed to start; %s", id, line)
		short = append(short, line)
		if v.cfg.KernelLog != nil {
			if k := v.cfg.KernelLog(in.ch.Pid()); len(k) > 0 {
				log.Printf("cloud-hypervisor: %s failed to start; the kernel logged:\n%s", id, strings.Join(k, "\n"))
				short = append(short, "kernel: "+k[len(k)-1])
			}
		}
	}
	if _, ok := logs["cloud-hypervisor.log"]; !ok {
		log.Printf("cloud-hypervisor: %s failed to start; cloud-hypervisor.log (its stdout and stderr) is empty", id)
		short = append(short, "cloud-hypervisor wrote nothing to its stdout or stderr")
	}
	names := make([]string, 0, len(logs))
	for n := range logs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		log.Printf("cloud-hypervisor: %s failed to start; %s ends:\n%s", id, n, logs[n])
	}
	if tail := logs["cloud-hypervisor.log"]; tail != "" {
		lines := strings.Split(tail, "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		short = append(short, "cloud-hypervisor.log ends: "+strings.Join(lines, " | "))
	}
	return strings.Join(short, "; ")
}

// KmsgLog reads the kernel's message buffer (/dev/kmsg, root only) and answers its last messages that name pid or
// tell of an OOM kill, a segfault, a seccomp or audit record or a killed process; nil when it cannot be read.
func KmsgLog(pid int) []string {
	return kmsgLog("/dev/kmsg", pid)
}

// kmsgLog is KmsgLog over the device at path.
func kmsgLog(path string, pid int) []string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	buf := make([]byte, 8192)
	needle := fmt.Sprintf("[%d]", pid)
	pidEq := fmt.Sprintf("pid=%d ", pid)
	for len(out) < 10000 {
		n, err := f.Read(buf)
		if err != nil {
			break // EAGAIN: the end of the buffer
		}
		rec := string(buf[:n])
		msg := rec
		if i := strings.IndexByte(rec, ';'); i >= 0 {
			msg = strings.TrimSpace(rec[i+1:])
		}
		low := strings.ToLower(msg)
		if strings.Contains(msg, needle) || strings.Contains(msg, pidEq) || strings.Contains(low, "cloud-hyperv") ||
			strings.Contains(low, "out of memory") || strings.Contains(low, "oom") || strings.Contains(low, "segfault") {
			out = append(out, msg)
		}
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

// logLines is the last n non-empty lines of a log, read from at most its last 16 KiB; empty when there is none.
func logLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-16<<10, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(buf), "\n") {
		if l = strings.TrimRight(l, "\r "); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
