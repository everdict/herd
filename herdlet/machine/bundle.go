package machine

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// A bundle is the snapshot object a herdlet writes to the snapshot store (ADR A-19): one tar holding first
// bundleManifest (the VMM's snapshot record, the template with its image references and container spec) and then
// the volume tree under volumes/<name>/. It is everything a VOLUMES move needs; a MEMORY snapshot's memory files
// stay where the VMM wrote them, so it restores only on that host.
const (
	bundleManifest = "herd-bundle.json"
	bundleVolumes  = "volumes"
	bundleVersion  = 1
)

// Manifest is the bundle's first entry.
type Manifest struct {
	Version  int           `json:"version"`
	Snapshot vmm.Snapshot  `json:"snapshot"`
	Template link.Template `json:"template"`
	// Images maps each image reference to the platform manifest digest it ran as on the producing host.
	Images map[string]string `json:"images,omitempty"`
	// Platform is the producing host's.
	Platform vmm.Platform `json:"platform"`
	Arch     string       `json:"arch"`
	// Network is the link a MEMORY snapshot's VM ran on (Config.Network): its memory keeps the address.
	Network *initd.Network `json:"network,omitempty"`
}

func marshalPlan(p *initd.Plan) ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("machine: plan: %w", err)
	}
	return b, nil
}

// writeBundle writes m and then the tree of every volume of the share. It does not follow symlinks: a link in a
// volume is stored as a link.
func writeBundle(w io.Writer, m Manifest, share string) error {
	tw := tar.NewWriter(w)
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("machine: bundle: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: bundleManifest, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	for _, v := range m.Template.Volumes {
		top := filepath.Join(share, v)
		fi, err := os.Lstat(top)
		if err != nil {
			return fmt.Errorf("machine: bundle: volume %q: %w", v, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: volume %q is a %v, not a directory", ErrOutsideShare, v, fi.Mode().Type())
		}
		err = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(share, p)
			if err != nil {
				return err
			}
			var link string
			if fi.Mode()&fs.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			}
			h, err := tar.FileInfoHeader(fi, link)
			if err != nil {
				return err
			}
			h.Name = bundleVolumes + "/" + filepath.ToSlash(rel)
			if fi.IsDir() {
				h.Name += "/"
			}
			h.Uname, h.Gname, h.Format = "", "", tar.FormatPAX
			if !fi.Mode().IsRegular() && !fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
				return nil // sockets, fifos and devices do not move
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				if _, err := io.CopyN(tw, f, h.Size); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("machine: bundle: volume %q: %w", v, err)
		}
	}
	return tw.Close()
}

// readManifest reads the bundle's first entry.
func readManifest(tr *tar.Reader) (Manifest, error) {
	h, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("machine: bundle: %w", err)
	}
	if h.Name != bundleManifest {
		return Manifest{}, fmt.Errorf("machine: bundle: first entry is %q, not %s", h.Name, bundleManifest)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("machine: bundle manifest: %w", err)
	}
	if m.Version != bundleVersion {
		return Manifest{}, fmt.Errorf("machine: bundle version %d, want %d", m.Version, bundleVersion)
	}
	return m, nil
}

// extractVolumes writes the rest of the bundle into the share through root. Every entry must lie in a declared
// volume; os.Root refuses any path that would leave the share, including through a symlink the bundle made, and an
// entry with a ".." element is refused before that.
func extractVolumes(tr *tar.Reader, root *os.Root, vols []string) error {
	declared := map[string]bool{}
	for _, v := range vols {
		declared[v] = true
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("machine: bundle: %w", err)
		}
		rel, ok := strings.CutPrefix(h.Name, bundleVolumes+"/")
		if !ok {
			return fmt.Errorf("machine: bundle: entry %q is not under %s/", h.Name, bundleVolumes)
		}
		rel = strings.TrimSuffix(rel, "/")
		for _, el := range strings.Split(rel, "/") {
			if el == ".." || el == "." || el == "" {
				return fmt.Errorf("%w: bundle entry %q", ErrOutsideShare, h.Name)
			}
		}
		if !declared[strings.Split(rel, "/")[0]] {
			return fmt.Errorf("machine: bundle: entry %q is in no declared volume", h.Name)
		}
		mode := fs.FileMode(h.Mode) & fs.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(rel, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return wrapEscape(h.Name, err)
			}
			if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
				return fmt.Errorf("%w: bundle entry %q is not a directory in the share", ErrOutsideShare, h.Name)
			}
		case tar.TypeReg:
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return wrapEscape(h.Name, err)
			}
			if _, err := io.CopyN(f, tr, h.Size); err != nil {
				_ = f.Close()
				return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
			}
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return wrapEscape(h.Name, err)
			}
		default:
			continue
		}
		// Owners come back where the herdlet may set them; elsewhere the guest sees the herdlet's.
		_ = root.Lchown(rel, h.Uid, h.Gid)
		if h.Typeflag != tar.TypeSymlink {
			_ = root.Chtimes(rel, h.ModTime, h.ModTime)
		}
	}
}

func wrapEscape(name string, err error) error {
	if strings.Contains(err.Error(), "escapes") || strings.Contains(err.Error(), "outside") {
		return fmt.Errorf("%w: bundle entry %q: %v", ErrOutsideShare, name, err)
	}
	return fmt.Errorf("machine: bundle: %s: %w", name, err)
}
