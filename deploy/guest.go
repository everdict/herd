package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// GuestIdentity names the guest build a herdlet boots: the SHA-256 of the SHA-256s of its kernel and its initramfs
// (herd-guest), each named by its role. It comes from the files themselves, so every herdlet of one release names the
// same guest wherever they are installed and a release that changes either names another. A golden snapshot records
// the guest that built it, and herd-api rebuilds one whose guest no connected worker runs any more (DEFAUL-1606).
func GuestIdentity(kernel, initrd string) (string, error) {
	h := sha256.New()
	for _, f := range []struct{ role, path string }{{"kernel", kernel}, {"initrd", initrd}} {
		d, err := fileDigest(f.path)
		if err != nil {
			return "", fmt.Errorf("guest %s: %w", f.role, err)
		}
		fmt.Fprintf(h, "%s sha256:%s\n", f.role, d)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
