package oci

import (
	"fmt"
	"slices"
	"strings"
)

// linuxCapabilities are the kernel's capabilities, CAP_CHOWN (0) through CAP_CHECKPOINT_RESTORE (40), by their
// names in include/uapi/linux/capability.h.
var linuxCapabilities = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL", "CAP_SETGID",
	"CAP_SETUID", "CAP_SETPCAP", "CAP_LINUX_IMMUTABLE", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST", "CAP_NET_ADMIN",
	"CAP_NET_RAW", "CAP_IPC_LOCK", "CAP_IPC_OWNER", "CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT",
	"CAP_SYS_PTRACE", "CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE", "CAP_SYS_RESOURCE",
	"CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD", "CAP_LEASE", "CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL",
	"CAP_SETFCAP", "CAP_MAC_OVERRIDE", "CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM", "CAP_BLOCK_SUSPEND",
	"CAP_AUDIT_READ", "CAP_PERFMON", "CAP_BPF", "CAP_CHECKPOINT_RESTORE",
}

// capabilityName is name as the OCI spec writes it: Kubernetes writes NET_RAW, Docker CAP_NET_RAW, and containerd
// takes either, in any case. ALL is answered as ALL.
func capabilityName(name string) (string, error) {
	up := strings.ToUpper(name)
	if up == "ALL" {
		return up, nil
	}
	if !strings.HasPrefix(up, "CAP_") {
		up = "CAP_" + up
	}
	if !slices.Contains(linuxCapabilities, up) {
		return "", fmt.Errorf("capability %q is not a Linux capability (write it as Kubernetes does, NET_RAW, or CAP_NET_RAW)", name)
	}
	return up, nil
}

// CheckCapabilities checks a container's capabilities.add and drop (Substrate's SecurityContext.capabilities): each
// a Linux capability with or without the CAP_ prefix; ALL only in drop.
func CheckCapabilities(add, drop []string) error {
	_, err := capabilitySets(add, drop)
	return err
}

// capabilitySets is a container process's capabilities: Docker's default set (none if drop holds ALL) and add, as
// its bounding, effective and permitted sets; add alone as its inheritable and ambient sets, so a non-root USER keeps
// what the template added across execve; and every capability in drop removed from all five, whether default or
// added. With neither list it is the default set exactly as before (nothing inheritable or ambient).
func capabilitySets(add, drop []string) (*Capabilities, error) {
	names := func(field string, in []string) ([]string, error) {
		var out []string
		for _, n := range in {
			c, err := capabilityName(n)
			if err != nil {
				return nil, fmt.Errorf("capabilities.%s: %w", field, err)
			}
			if c == "ALL" && field == "add" {
				return nil, fmt.Errorf("capabilities.add: ALL is not granted; name the capabilities the container needs")
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		return out, nil
	}
	adds, err := names("add", add)
	if err != nil {
		return nil, err
	}
	drops, err := names("drop", drop)
	if err != nil {
		return nil, err
	}
	var base []string
	if !slices.Contains(drops, "ALL") {
		base = slices.Clone(DefaultCapabilities)
	}
	kept := func(caps []string) []string {
		var out []string
		for _, c := range caps {
			if !slices.Contains(drops, c) && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		return out
	}
	full, added := kept(append(base, adds...)), kept(adds)
	c := &Capabilities{Bounding: full, Effective: slices.Clone(full), Permitted: slices.Clone(full)}
	if len(added) > 0 {
		c.Inheritable, c.Ambient = added, slices.Clone(added)
	}
	return c, nil
}
