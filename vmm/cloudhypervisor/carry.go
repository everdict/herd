//go:build linux

package cloudhypervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/everdict/herd/vmm"
)

var _ vmm.SnapshotCarrier = (*VMM)(nil)

// fileRE is a file name Cloud Hypervisor or herd writes into a snapshot directory (config.json, state.json,
// memory-ranges, herd-snapshot.json).
var fileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ExportSnapshot lists the files of the published snapshot s: what Cloud Hypervisor wrote (config, device state,
// memory) and herd's record of it, which ImportSnapshot checks on the other side.
func (v *VMM) ExportSnapshot(ctx context.Context, s vmm.Snapshot) ([]vmm.SnapshotFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: export: %w", err)
	}
	dir, err := v.snapshotDir(s)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: export %s: %w", s.ID, err)
	}
	var files []vmm.SnapshotFile
	for _, e := range ents {
		if !e.Type().IsRegular() || !fileRE.MatchString(e.Name()) {
			return nil, fmt.Errorf("cloud-hypervisor: export %s: %s is not a file a snapshot holds", s.ID, e.Name())
		}
		fi, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("cloud-hypervisor: export %s: %w", s.ID, err)
		}
		p := filepath.Join(dir, e.Name())
		files = append(files, vmm.SnapshotFile{Name: e.Name(), Size: fi.Size(), Open: func() (io.ReadCloser, error) { return os.Open(p) }})
	}
	return files, nil
}

// ImportSnapshot writes the files into a partial directory, checks herd's record among them names s, and publishes
// the directory under s's name, as Snapshot does: Restore finds it as if this host had taken it. A snapshot s already
// published here is kept; two imports of one snapshot at once publish one of them.
func (v *VMM) ImportSnapshot(ctx context.Context, s vmm.Snapshot, next func() (vmm.SnapshotFile, io.Reader, error)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cloud-hypervisor: import: %w", err)
	}
	if _, err := v.snapshotDir(s); err == nil {
		return nil
	} else if !errors.Is(err, vmm.ErrNotFound) || !idRE.MatchString(s.ID) {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("cloud-hypervisor: import: %w", err)
	}
	tmp := filepath.Join(v.cfg.SnapshotDir, ".import-"+s.ID+"-"+hex.EncodeToString(rnd[:]))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("cloud-hypervisor: import: %w", err)
	}
	if err := v.importInto(ctx, s, tmp, next); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func (v *VMM) importInto(ctx context.Context, s vmm.Snapshot, tmp string, next func() (vmm.SnapshotFile, io.Reader, error)) error {
	for {
		f, r, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("cloud-hypervisor: import %s: %w", s.ID, err)
		}
		if !fileRE.MatchString(f.Name) {
			return fmt.Errorf("cloud-hypervisor: import %s: %q is not a file a snapshot holds", s.ID, f.Name)
		}
		if err := writeFrom(filepath.Join(tmp, f.Name), r, f.Size); err != nil {
			return fmt.Errorf("cloud-hypervisor: import %s: %s: %w", s.ID, f.Name, err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cloud-hypervisor: import %s: %w", s.ID, err)
		}
	}
	if err := syncDir(tmp); err != nil {
		return fmt.Errorf("cloud-hypervisor: import %s: %w", s.ID, err)
	}
	// The record must be the reported snapshot's, checked before publishing: a mismatch publishes nothing.
	if err := checkMeta(tmp, s); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cloud-hypervisor: import %s: no %s among the files", s.ID, metaFile)
		}
		return fmt.Errorf("cloud-hypervisor: import: %w", err)
	}
	final := filepath.Join(v.cfg.SnapshotDir, s.ID)
	if err := os.Rename(tmp, final); err != nil {
		if _, perr := v.snapshotDir(s); perr == nil {
			_ = os.RemoveAll(tmp) // another import of s published first
			return nil
		}
		return fmt.Errorf("cloud-hypervisor: import %s: publish: %w", s.ID, err)
	}
	return syncDir(v.cfg.SnapshotDir)
}

// writeFrom writes exactly size bytes of r to a new file at path and syncs it.
func writeFrom(path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("carried twice")
		}
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
