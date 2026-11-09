package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/execrec"
	"github.com/everdict/herd/guest/layer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sync writes what the guest buffered back to its devices and the host (PLT-195): syncfs(2) on each container's root
// and on every directory in SyncDirs (the volume share), which flushes exactly those filesystems and reports their
// write-back errors. herdlet calls it before every snapshot. It answers when ctx ends even if a flush is still
// blocked (an unanswering virtio-fs server), so the caller's deadline decides and the snapshot is not taken.
// With LayerShare, it first saves each container's writable layer there (DEFAUL-1619), leaving out its exec records:
// a cold boot runs none of the processes they name.
// After a successful flush it drops the guest's dentries and inodes (ADR A-52): a snapshot then holds no cached
// virtio-fs entry that the virtiofsd started at restore would have to re-find by path. The drop is not a durability
// step, so its failure is logged and the Sync still succeeds; like the flush, it answers ctx.
func (g *Guest) Sync(ctx context.Context, _ *herdext.SyncRequest) (*herdext.SyncResponse, error) {
	dirs := append([]string(nil), g.SyncDirs...)
	for _, c := range g.containers {
		dirs = append(dirs, c.t.Rootfs())
	}
	flush := g.SyncFS
	if flush == nil {
		flush = syncFS
	}
	drop := g.DropCaches
	if drop == nil {
		drop = dropCaches
	}
	done := make(chan error, 1)
	go func() {
		if err := g.saveLayers(); err != nil {
			done <- err
			return
		}
		if err := syncDirs(dirs, flush); err != nil {
			done <- err
			return
		}
		if err := drop(); err != nil {
			log.Printf("herd-guest: sync: dropping the dentry and inode caches: %v", err)
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, status.Errorf(codes.Internal, "sync: %v", err)
		}
		return &herdext.SyncResponse{}, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

func (g *Guest) saveLayers() error {
	if g.LayerShare == "" {
		return nil
	}
	for _, c := range g.containers {
		lt, ok := c.t.(LayerTarget)
		if !ok || len(lt.Layers()) == 0 {
			continue
		}
		// The records are under the root, so in its upper layer (or, for a Local target, its one directory).
		var skip []string
		for name := range lt.Layers() {
			skip = append(skip, name+execrec.Dir)
		}
		if err := layer.Save(g.LayerShare, c.t.Name(), lt.Layers(), skip); err != nil {
			return fmt.Errorf("saving container %q's writable layer: %w", c.t.Name(), err)
		}
	}
	return nil
}

func syncDirs(dirs []string, flush func(*os.File) error) error {
	var errs []error
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := flush(f); err != nil {
			errs = append(errs, fmt.Errorf("syncfs %s: %w", d, err))
		}
		f.Close()
	}
	return errors.Join(errs...)
}
