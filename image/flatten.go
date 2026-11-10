package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// entry is one path of the flattened tree.
type entry struct {
	hdr   tar.Header
	layer int
	// off is where a regular file's bytes start in the spool.
	off int64
}

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// node is one path of the tree being built: its entry, or nil for a parent no entry has named, and its children by
// name. Removing a path drops its node, and every path under it with it, without looking at the rest of the tree.
type node struct {
	e    *entry
	kids map[string]*node
}

// find is the node at name, or nil.
func (n *node) find(name string) *node {
	for _, el := range strings.Split(name, "/") {
		if n = n.kids[el]; n == nil {
			return nil
		}
	}
	return n
}

// make is the node at name, adding it and its parents.
func (n *node) make(name string) *node {
	for _, el := range strings.Split(name, "/") {
		k := n.kids[el]
		if k == nil {
			if n.kids == nil {
				n.kids = map[string]*node{}
			}
			k = &node{}
			n.kids[el] = k
		}
		n = k
	}
	return n
}

// remove drops name and everything under it.
func (n *node) remove(name string) {
	dir, base := path.Split(name)
	if dir != "" {
		if n = n.find(strings.TrimSuffix(dir, "/")); n == nil {
			return
		}
	}
	delete(n.kids, base)
}

// opaque drops every entry under n from a layer below li.
func (n *node) opaque(li int) {
	for _, c := range n.kids {
		if c.e != nil && c.e.layer < li {
			c.e = nil
		}
		c.opaque(li)
	}
}

// collect puts every entry under n, at path p, into tree, a parent no entry named becoming a plain directory when
// there is an entry under it, and reports whether it put any.
func (n *node) collect(p string, tree map[string]*entry) bool {
	found := false
	for k, c := range n.kids {
		cp := k
		if p != "" {
			cp = p + "/" + k
		}
		if c.collect(cp, tree) && c.e == nil {
			tree[cp] = &entry{hdr: tar.Header{Typeflag: tar.TypeDir, Name: cp, Mode: 0o755, Format: tar.FormatPAX}}
		}
		if c.e != nil {
			tree[cp] = c.e
		}
		if tree[cp] != nil {
			found = true
		}
	}
	return found
}

// cleanName turns a tar entry name into a relative, clean path, or refuses it: absolute names lose their leading
// slash, and a name with a ".." element is a bad layer (it would escape the tree).
func cleanName(name string) (string, error) {
	n := strings.TrimLeft(name, "/")
	for _, el := range strings.Split(n, "/") {
		if el == ".." {
			return "", fmt.Errorf("%w: entry %q leaves the tree", ErrBadLayer, name)
		}
	}
	n = path.Clean(n)
	if n == "." {
		return "", nil
	}
	return n, nil
}

// Flatten applies the layers in order (lowest first) with OCI whiteouts and writes the resulting tree as one tar to
// w: every path once, parents before children, hard links last so their target precedes them. Regular files'
// bytes are spooled in a temporary file in tmpDir. Owners, modes, times, device numbers and xattrs are kept, so the
// filesystem tool builds the tree without root.
func Flatten(layers []io.Reader, w io.Writer, tmpDir string) error {
	spool, err := os.CreateTemp(tmpDir, "flatten-*")
	if err != nil {
		return fmt.Errorf("image: flatten: %w", err)
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	root := &node{}
	// linksTo holds, by target, the paths of hard links that may still point at it.
	linksTo := map[string]map[string]struct{}{}
	var off int64
	for li, l := range layers {
		tr := tar.NewReader(l)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("%w: layer %d: %v", ErrBadLayer, li, err)
			}
			name, err := cleanName(h.Name)
			if err != nil {
				return err
			}
			if name == "" {
				continue
			}
			dir, base := path.Split(name)
			dir = strings.TrimSuffix(dir, "/")
			switch {
			case base == opaqueMarker:
				d := root
				if dir != "" {
					d = root.find(dir)
				}
				if d != nil {
					d.opaque(li)
				}
				continue
			case strings.HasPrefix(base, whiteoutPrefix):
				root.remove(path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))
				continue
			}
			e := &entry{hdr: *h, layer: li}
			e.hdr.Name = name
			e.hdr.Uname, e.hdr.Gname = "", ""
			e.hdr.Format = tar.FormatPAX
			e.hdr.AccessTime, e.hdr.ChangeTime = e.hdr.ModTime, e.hdr.ModTime
			e.hdr.PAXRecords = xattrsOnly(h.PAXRecords)
			switch h.Typeflag {
			case tar.TypeReg, tar.TypeRegA:
				e.hdr.Typeflag = tar.TypeReg
				n, err := io.Copy(spool, tr)
				if err != nil {
					return fmt.Errorf("image: flatten: spooling %s: %w", name, err)
				}
				e.off, e.hdr.Size = off, n
				off += n
			case tar.TypeLink:
				target, err := cleanName(h.Linkname)
				if err != nil || target == "" {
					return fmt.Errorf("%w: hard link %q → %q", ErrBadLayer, h.Name, h.Linkname)
				}
				e.hdr.Linkname, e.hdr.Size = target, 0
				if linksTo[target] == nil {
					linksTo[target] = map[string]struct{}{}
				}
				linksTo[target][name] = struct{}{}
			case tar.TypeDir, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
				e.hdr.Size = 0
			default:
				continue
			}
			n := root.make(name)
			if old := n.e; old != nil && old.hdr.Typeflag == tar.TypeDir && e.hdr.Typeflag == tar.TypeDir {
				// A directory entry over a directory changes its metadata and keeps its children.
				n.e = e
				continue
			}
			if old := n.e; old != nil && old.hdr.Typeflag == tar.TypeReg {
				// Hard links to the replaced file keep its old contents, as they would on a layered filesystem.
				for lp := range linksTo[name] {
					if ln := root.find(lp); ln != nil && ln.e != nil {
						if l := ln.e; l.hdr.Typeflag == tar.TypeLink && l.hdr.Linkname == name {
							l.hdr.Typeflag, l.hdr.Linkname, l.hdr.Size, l.off = tar.TypeReg, "", old.hdr.Size, old.off
							l.hdr.Mode, l.hdr.Uid, l.hdr.Gid = old.hdr.Mode, old.hdr.Uid, old.hdr.Gid
						}
					}
				}
				delete(linksTo, name)
			}
			n.e, n.kids = e, nil
		}
	}
	// Implicit parents become plain directories.
	tree := map[string]*entry{}
	root.collect("", tree)
	var names, links []string
	for p, e := range tree {
		if e.hdr.Typeflag == tar.TypeLink {
			if t, ok := tree[e.hdr.Linkname]; !ok || t.hdr.Typeflag != tar.TypeReg {
				continue // its target was removed by an upper layer
			}
			links = append(links, p)
			continue
		}
		names = append(names, p)
	}
	sort.Strings(names)
	sort.Strings(links)
	tw := tar.NewWriter(w)
	for _, p := range append(names, links...) {
		e := tree[p]
		h := e.hdr
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		if err := tw.WriteHeader(&h); err != nil {
			return fmt.Errorf("image: flatten: %s: %w", p, err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if _, err := io.Copy(tw, io.NewSectionReader(spool, e.off, h.Size)); err != nil {
				return fmt.Errorf("image: flatten: %s: %w", p, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("image: flatten: %w", err)
	}
	return nil
}

func xattrsOnly(pax map[string]string) map[string]string {
	var out map[string]string
	for k, v := range pax {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// Listing reads a tar and lists it one entry per line, "<type><mode> <uid>:<gid> <size> <path>[ -> <target>]", in
// the tar's order. It is what the golden tests compare.
func Listing(r io.Reader) (string, error) {
	tr := tar.NewReader(r)
	var b strings.Builder
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		t := map[byte]string{tar.TypeReg: "-", tar.TypeDir: "d", tar.TypeSymlink: "l", tar.TypeLink: "h",
			tar.TypeChar: "c", tar.TypeBlock: "b", tar.TypeFifo: "p"}[h.Typeflag]
		if t == "" {
			t = "?"
		}
		fmt.Fprintf(&b, "%s%04o %d:%d %d %s", t, h.Mode&0o7777, h.Uid, h.Gid, h.Size, h.Name)
		if h.Linkname != "" {
			fmt.Fprintf(&b, " -> %s", h.Linkname)
		}
		b.WriteByte('\n')
	}
}
