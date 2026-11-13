//go:build linux

package cloudhypervisor

import (
	"bytes"
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
	"strings"

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

// writeFrom writes exactly size bytes of r to a new file at path and syncs it. Runs of zeros are left as holes
// (PLT-591): a memory file is mostly zero pages, which a restore then neither writes nor stores.
func writeFrom(path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("carried twice")
		}
		return err
	}
	if err := copySparse(f, r, size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

var _ vmm.SnapshotRemover = (*VMM)(nil)

// LocalSnapshots lists the snapshots published in the snapshot directory: each directory named as a snapshot id that
// holds herd's record. Partial and import directories are not published and not listed.
func (v *VMM) LocalSnapshots(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: snapshots: %w", err)
	}
	ents, err := os.ReadDir(v.cfg.SnapshotDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: snapshots: %w", err)
	}
	var ids []string
	for _, e := range ents {
		if !e.IsDir() || !idRE.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(v.cfg.SnapshotDir, e.Name(), metaFile)); err == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// RemoveSnapshot removes the snapshot directory of id. Its record goes first, so a removal cut short leaves no
// directory Restore or ImportSnapshot would take for the snapshot.
func (v *VMM) RemoveSnapshot(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cloud-hypervisor: remove snapshot: %w", err)
	}
	if !idRE.MatchString(id) {
		return fmt.Errorf("cloud-hypervisor: remove snapshot %q: not a snapshot id", id)
	}
	dir := filepath.Join(v.cfg.SnapshotDir, id)
	if err := os.Remove(filepath.Join(dir, metaFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cloud-hypervisor: remove snapshot %s: %w", id, err)
	}
	if err := v.removeAll(dir); err != nil {
		return fmt.Errorf("cloud-hypervisor: remove snapshot %s: %w", id, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cloud-hypervisor: remove snapshot %s: still there (%v)", id, err)
	}
	return nil
}

// RemoveLeftovers removes the .partial- directories of snapshots Snapshot never published, the .import- directories
// of imports that never finished, and snapshot directories without herd's record (a removal cut short).
func (v *VMM) RemoveLeftovers(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cloud-hypervisor: leftovers: %w", err)
	}
	ents, err := os.ReadDir(v.cfg.SnapshotDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cloud-hypervisor: leftovers: %w", err)
	}
	var errs []error
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(v.cfg.SnapshotDir, name)
		switch {
		case !e.IsDir():
			continue
		case strings.HasPrefix(name, ".partial-"), strings.HasPrefix(name, ".import-"):
		case idRE.MatchString(name):
			if _, err := os.Lstat(filepath.Join(p, metaFile)); !errors.Is(err, fs.ErrNotExist) {
				continue
			}
		default:
			continue
		}
		if err := v.removeAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cloud-hypervisor: leftovers: %w", err)
	}
	return nil
}

// copySparse copies exactly size bytes of r to f, from its start, seeking over each 64 KiB block of r that is all
// zeros rather than writing it; the file ends at size.
func copySparse(f *os.File, r io.Reader, size int64) error {
	buf := make([]byte, len(zeroBlock))
	for n := int64(0); n < size; {
		b := buf[:min(int64(len(buf)), size-n)]
		if _, err := io.ReadFull(r, b); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if allZero(b) {
			if _, err := f.Seek(int64(len(b)), io.SeekCurrent); err != nil {
				return err
			}
		} else if _, err := f.Write(b); err != nil {
			return err
		}
		n += int64(len(b))
	}
	return f.Truncate(size)
}

var zeroBlock = make([]byte, 64<<10)

func allZero(b []byte) bool { return bytes.Equal(b, zeroBlock[:len(b)]) }
