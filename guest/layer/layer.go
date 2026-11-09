// Package layer carries a container's writable layer through a snapshot (DEFAUL-1619). A container's root
// filesystem is its image under an overlay whose upper directory, like the directory bound at /tmp, lives in the
// guest's tmpfs: a snapshot that keeps no memory would lose them, and an actor cold-booted from it would see the bare
// image. herd-guest's Sync therefore archives each container's layer into the share (Save), the herdlet's bundle
// carries the archive beside the volumes, and a cold boot puts it back before the overlay is mounted (Restore).
//
// An archive is a tar of entries <name>/<path> for each named directory (initd: "upper" and "tmp"). It keeps what
// an overlay upper holds: modes, owners, times, symlinks, devices (a whiteout is a 0/0 character device) and extended
// attributes (an opaque directory's trusted.overlay.opaque) where the platform has them. Hard links are stored as
// separate files.
package layer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Dir is where, relative to the share, the guest leaves each container's archive: Dir/<container>.tar.
const Dir = ".herd/rootfs"

// Ext ends every archive's name in Dir.
const Ext = ".tar"

// Path is container's archive relative to the share.
func Path(container string) string { return path.Join(Dir, container+Ext) }

const xattrPrefix = "SCHILY.xattr."

// Write archives each directory of dirs (archive name → directory) into w. An entry whose name (<name>/<path>) is
// in skip, or below one, is left out with everything under it. Sockets are left out; nothing listens on one after
// a boot.
//
// The directories are live (a container keeps running while its layer is saved), so a file the walk listed may be
// gone when Write reaches it, as Postgres removes postmaster.pid while it starts (PLT-380). ENOENT where Write reads
// a directory, an entry's information, a symlink's target, its extended attributes or a file's body means that file
// is not part of the layer, and Write goes on; any other error fails it. A regular file is opened before its header
// is written, so one that vanishes is left out whole and never leaves a header without its body.
func Write(w io.Writer, dirs map[string]string, skip []string) error {
	tw := tar.NewWriter(w)
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		top := dirs[n]
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p != top && vanished(err) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(top, p)
			if err != nil || rel == "." {
				return err
			}
			name := n + "/" + filepath.ToSlash(rel)
			if skipped(name, skip) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			fi, err := d.Info()
			if vanished(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if fi.Mode()&fs.ModeSocket != 0 {
				return nil
			}
			var link string
			if fi.Mode()&fs.ModeSymlink != 0 {
				if link, err = os.Readlink(p); vanished(err) {
					return nil
				} else if err != nil {
					return err
				}
			}
			var f *os.File
			if fi.Mode().IsRegular() {
				if f, err = os.Open(p); vanished(err) {
					return nil
				} else if err != nil {
					return err
				}
				defer f.Close()
			}
			h, err := tar.FileInfoHeader(fi, link)
			if err != nil {
				return err
			}
			h.Name, h.Uname, h.Gname, h.Format = name, "", "", tar.FormatPAX
			if fi.IsDir() {
				h.Name += "/"
			}
			xa, err := listXattrs(p)
			if vanished(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("extended attributes of %s: %w", p, err)
			}
			for k, v := range xa {
				if h.PAXRecords == nil {
					h.PAXRecords = map[string]string{}
				}
				h.PAXRecords[xattrPrefix+k] = v
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if f != nil {
				if _, err := io.CopyN(tw, f, h.Size); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("layer: %s: %w", n, err)
		}
	}
	return tw.Close()
}

// vanished reports whether err says the file is gone (ENOENT).
func vanished(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func skipped(name string, skip []string) bool {
	for _, s := range skip {
		if name == s || strings.HasPrefix(name, s+"/") {
			return true
		}
	}
	return false
}

// Extract writes an archive Write made into dirs: an entry <name>/<path> goes to <path> under dirs[name], through
// os.Root, so no entry (nor a symlink one made) reaches outside its directory; an entry with a ".." element is
// refused, one of a name dirs does not hold is skipped. Owners, modes, times and extended attributes are set as the
// archive holds them; directories get their modes last, so a read-only one is still filled.
func Extract(r io.Reader, dirs map[string]string) error {
	roots := map[string]*os.Root{}
	defer func() {
		for _, rt := range roots {
			rt.Close()
		}
	}()
	type dirMode struct {
		root *os.Root
		rel  string
		h    *tar.Header
	}
	var later []dirMode
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("layer: %w", err)
		}
		n, rel, ok := strings.Cut(strings.TrimSuffix(h.Name, "/"), "/")
		if !ok || rel == "" {
			return fmt.Errorf("layer: entry %q names no path", h.Name)
		}
		for _, el := range strings.Split(rel, "/") {
			if el == ".." || el == "." || el == "" {
				return fmt.Errorf("layer: entry %q leaves its directory", h.Name)
			}
		}
		dir, ok := dirs[n]
		if !ok {
			continue
		}
		root := roots[n]
		if root == nil {
			if root, err = os.OpenRoot(dir); err != nil {
				return fmt.Errorf("layer: %w", err)
			}
			roots[n] = root
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
			if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
				return fmt.Errorf("layer: %s is not a directory", h.Name)
			}
			later = append(later, dirMode{root, rel, h})
			continue
		case tar.TypeReg:
			_ = root.Remove(rel)
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
			if _, err := io.CopyN(f, tr, h.Size); err != nil {
				_ = f.Close()
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
		case tar.TypeSymlink:
			_ = root.Remove(rel)
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			_ = root.Remove(rel)
			if err := mknod(root, rel, h); err != nil {
				return fmt.Errorf("layer: %s: %w", h.Name, err)
			}
		default:
			continue
		}
		if err := finish(root, rel, h); err != nil {
			return fmt.Errorf("layer: %s: %w", h.Name, err)
		}
	}
	for i := len(later) - 1; i >= 0; i-- {
		if err := finish(later[i].root, later[i].rel, later[i].h); err != nil {
			return fmt.Errorf("layer: %s: %w", later[i].h.Name, err)
		}
	}
	return nil
}

// finish gives rel the owner, extended attributes, mode and times h holds. The owner is set where the extractor may
// (herd-guest is root); elsewhere the files are the extractor's.
func finish(root *os.Root, rel string, h *tar.Header) error {
	_ = root.Lchown(rel, h.Uid, h.Gid)
	for k, v := range h.PAXRecords {
		if name, ok := strings.CutPrefix(k, xattrPrefix); ok {
			if err := setXattr(root, rel, name, v); err != nil {
				return fmt.Errorf("extended attribute %s: %w", name, err)
			}
		}
	}
	if h.Typeflag == tar.TypeSymlink {
		return nil
	}
	// After the owner: a chown clears the set-id bits.
	if err := root.Chmod(rel, mode(h.Mode)); err != nil {
		return err
	}
	return root.Chtimes(rel, h.ModTime, h.ModTime)
}

func mode(m int64) fs.FileMode {
	out := fs.FileMode(m) & fs.ModePerm
	if m&0o4000 != 0 {
		out |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		out |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		out |= fs.ModeSticky
	}
	return out
}

// Save archives container's dirs (Write) into share at Path(container), whole or not at all: it is written beside
// and renamed into place, so a cold boot never reads half an archive.
func Save(share, container string, dirs map[string]string, skip []string) error {
	root, err := os.OpenRoot(share)
	if err != nil {
		return fmt.Errorf("layer: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(Dir, 0o755); err != nil {
		return fmt.Errorf("layer: %w", err)
	}
	dst := Path(container)
	tmp := dst + ".partial"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("layer: %w", err)
	}
	if err := Write(f, dirs, skip); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("layer: %w", err)
	}
	if err := root.Rename(tmp, dst); err != nil {
		return fmt.Errorf("layer: %w", err)
	}
	return nil
}

// Restore extracts container's archive in share into dirs (Extract) and answers whether there was one. A share
// with none (a new actor, or a snapshot taken before herd carried layers) leaves dirs as they are.
func Restore(share, container string, dirs map[string]string) (bool, error) {
	root, err := os.OpenRoot(share)
	if err != nil {
		return false, fmt.Errorf("layer: %w", err)
	}
	defer root.Close()
	f, err := root.Open(Path(container))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("layer: %w", err)
	}
	defer f.Close()
	if err := Extract(f, dirs); err != nil {
		return true, fmt.Errorf("layer: container %q: %w", container, err)
	}
	return true, nil
}
