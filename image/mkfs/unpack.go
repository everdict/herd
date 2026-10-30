package mkfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// unpack writes a flattened tar (image.Flatten's output) into dir through os.Root, so no entry lands outside it.
// Devices and fifos are skipped; owners are set where the process may set them. Directory modes are set last,
// deepest first, so a read-only directory does not stop its own children.
func unpack(tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	type dirMode struct {
		name string
		mode fs.FileMode
	}
	var dirs []dirMode
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("mkfs: unpack: %w", err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		mode := fs.FileMode(h.Mode) & fs.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			var out *os.File
			if out, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
				_, err = io.CopyN(out, tr, h.Size)
				if cerr := out.Close(); err == nil {
					err = cerr
				}
			}
		case tar.TypeSymlink:
			err = root.Symlink(h.Linkname, name)
		case tar.TypeLink:
			err = root.Link(h.Linkname, name)
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("mkfs: unpack %s: %w", h.Name, err)
		}
		_ = root.Lchown(name, h.Uid, h.Gid)
		switch h.Typeflag {
		case tar.TypeDir:
			dirs = append(dirs, dirMode{name, mode})
		case tar.TypeReg:
			if err := root.Chmod(name, mode); err != nil {
				return fmt.Errorf("mkfs: unpack %s: %w", h.Name, err)
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
			return fmt.Errorf("mkfs: unpack %s: %w", dirs[i].name, err)
		}
	}
	return nil
}

// removeTree removes an unpacked tree, making its directories writable first (an image may hold read-only ones).
func removeTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
