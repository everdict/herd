//go:build linux

package cloudhypervisor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// CPU is the boundary to the host processor: what a MEMORY snapshot's restore depends on.
type CPU interface {
	// Arch is amd64 or arm64.
	Arch() string
	// Class is the lower-case CPU feature class label of this host (ADR A-8). Two hosts with the same class must be
	// able to restore each other's memory snapshots; hosts whose checked features differ get different classes.
	Class() (string, error)
}

// CPUIDFunc executes CPUID for a leaf and sub-leaf.
type CPUIDFunc func(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)

// X86CPU derives the class from the CPUID words Cloud Hypervisor compares on restore. The R0 spike saw v54 refuse a
// restore across Intel generations on exactly these ("Detected incompatible CPUID entry": leaf 0x1 ECX and leaf 0x7
// EBX/ECX/EDX, checked as a bitwise subset). The class is the vendor plus a digest of those words: equal words give
// equal classes, any differing bit gives a different one. It is deliberately stricter than CH's subset rule (a host
// that is a strict superset could restore, but declares its own family).
type X86CPU struct {
	CPUID CPUIDFunc
}

func (X86CPU) Arch() string { return "amd64" }

func (c X86CPU) Class() (string, error) {
	if c.CPUID == nil {
		return "", fmt.Errorf("cloud-hypervisor: no CPUID source")
	}
	maxLeaf, b, cx, d := c.CPUID(0, 0)
	if maxLeaf < 7 {
		return "", fmt.Errorf("cloud-hypervisor: CPUID max leaf %#x has no leaf 7", maxLeaf)
	}
	var vendor [12]byte
	binary.LittleEndian.PutUint32(vendor[0:], b)
	binary.LittleEndian.PutUint32(vendor[4:], d)
	binary.LittleEndian.PutUint32(vendor[8:], cx)
	_, _, ecx1, _ := c.CPUID(1, 0)
	_, ebx7, ecx7, edx7 := c.CPUID(7, 0)
	return featureClass(x86VendorLabel(string(vendor[:])), "cpuid-v1", string(vendor[:]), ecx1, ebx7, ecx7, edx7), nil
}

func x86VendorLabel(vendor string) string {
	switch vendor {
	case "GenuineIntel":
		return "intel"
	case "AuthenticAMD":
		return "amd"
	}
	return "x86"
}

// featureClass is prefix-<10 hex digits of sha256 over a scheme name, an identity string and the feature words>.
func featureClass(prefix, scheme, ident string, words ...uint32) string {
	h := sha256.New()
	h.Write([]byte(scheme))
	h.Write([]byte{0})
	h.Write([]byte(ident))
	h.Write([]byte{0})
	var w [4]byte
	for _, x := range words {
		binary.LittleEndian.PutUint32(w[:], x)
		h.Write(w[:])
	}
	return prefix + "-" + hex.EncodeToString(h.Sum(nil)[:5])
}

// ARM64CPU derives the class from MIDR_EL1 (implementer, part, revision) and the kernel's feature list. Cloud
// Hypervisor has no CPUID check on arm64; the class keeps memory restores between identical cores.
type ARM64CPU struct {
	// Root is prepended to /sys and /proc paths (tests); empty on a host.
	Root string
}

func (ARM64CPU) Arch() string { return "arm64" }

func (c ARM64CPU) Class() (string, error) {
	midr, err := os.ReadFile(c.Root + "/sys/devices/system/cpu/cpu0/regs/identification/midr_el1")
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor: read MIDR_EL1: %w", err)
	}
	info, err := os.ReadFile(c.Root + "/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor: read cpuinfo: %w", err)
	}
	var features string
	sc := bufio.NewScanner(bytes.NewReader(info))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "Features" {
			features = strings.Join(strings.Fields(v), " ")
			break
		}
	}
	if features == "" {
		return "", fmt.Errorf("cloud-hypervisor: cpuinfo has no Features line")
	}
	return featureClass("arm", "midr-features-v1", strings.TrimSpace(string(midr))+"\x00"+features), nil
}
