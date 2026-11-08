//go:build linux

package layer

import (
	"archive/tar"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// listXattrs reads p's extended attributes (not a symlink's target's); a filesystem without them answers none.
func listXattrs(p string) (map[string]string, error) {
	sz, err := unix.Llistxattr(p, nil)
	if errors.Is(err, unix.ENOTSUP) || sz == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sz)
	if sz, err = unix.Llistxattr(p, buf); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsz, err := unix.Lgetxattr(p, name, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		v := make([]byte, vsz)
		if vsz, err = unix.Lgetxattr(p, name, v); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = string(v[:vsz])
	}
	return out, nil
}

// inDir runs f on rel's directory (opened through root, so it is inside it) by its /proc/self/fd path, and rel's
// last element.
func inDir(root *os.Root, rel string, f func(dir *os.File, base string) error) error {
	d, err := root.Open(path.Dir(rel))
	if err != nil {
		return err
	}
	defer d.Close()
	return f(d, path.Base(rel))
}

// setXattr sets an extended attribute on rel itself. Only the overlay's own attributes (an opaque directory, a
// redirect) are required: another the guest cannot set (a security label of the producing host) is left out.
func setXattr(root *os.Root, rel, name, value string) error {
	err := inDir(root, rel, func(d *os.File, base string) error {
		return unix.Lsetxattr(fmt.Sprintf("/proc/self/fd/%d/%s", d.Fd(), base), name, []byte(value), 0)
	})
	if err != nil && !strings.HasPrefix(name, "trusted.overlay.") {
		return nil
	}
	return err
}

// mknod makes the device or fifo h names at rel (an overlay whiteout is a character device 0/0).
func mknod(root *os.Root, rel string, h *tar.Header) error {
	m := uint32(h.Mode) & 0o7777
	switch h.Typeflag {
	case tar.TypeChar:
		m |= unix.S_IFCHR
	case tar.TypeBlock:
		m |= unix.S_IFBLK
	case tar.TypeFifo:
		m |= unix.S_IFIFO
	}
	return inDir(root, rel, func(d *os.File, base string) error {
		return unix.Mknodat(int(d.Fd()), base, m, int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor))))
	})
}
