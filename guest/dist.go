package guest

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Arches are the architectures herd builds its guest for; build.sh all builds both into one directory
// (herd-guest-<arch>/{kernel,initrd.img}), and each host installs its own (herdlet doctor checks it with KernelArch).
var Arches = []string{"amd64", "arm64"}

// KernelArch reads a kernel image's architecture from its boot header: the arm64 Image header's magic "ARM\x64" at
// offset 56, or the x86 boot protocol's "HdrS" at offset 0x202 (bzImage).
func KernelArch(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("guest: %w", err)
	}
	defer f.Close()
	h := make([]byte, 0x206)
	n, err := io.ReadFull(f, h)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("guest: %s: %w", path, err)
	}
	h = h[:n]
	switch {
	case len(h) >= 60 && string(h[56:60]) == "ARM\x64":
		return "arm64", nil
	case len(h) >= 0x206 && string(h[0x202:0x206]) == "HdrS":
		return "amd64", nil
	}
	return "", fmt.Errorf("guest: %s is neither an arm64 Image nor an x86 bzImage", path)
}
