package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	herdext "github.com/everdict/herd/api/herdext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sync writes what the guest buffered back to its devices and the host (PLT-195): sync(2) for every filesystem the
// guest holds, then syncfs(2) on each container's root and on every directory in SyncDirs, whose write-back errors
// sync(2) does not report. herdlet calls it before every snapshot. It answers when ctx ends even if a flush is still
// blocked (an unanswering virtio-fs server), so the caller's deadline decides and the snapshot is not taken.
func (g *Guest) Sync(ctx context.Context, _ *herdext.SyncRequest) (*herdext.SyncResponse, error) {
	dirs := append([]string(nil), g.SyncDirs...)
	for _, c := range g.containers {
		dirs = append(dirs, c.t.Rootfs())
	}
	done := make(chan error, 1)
	go func() { done <- syncDirs(dirs) }()
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

func syncDirs(dirs []string) error {
	syncAll()
	var errs []error
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := syncFS(f); err != nil {
			errs = append(errs, fmt.Errorf("syncfs %s: %w", d, err))
		}
		f.Close()
	}
	return errors.Join(errs...)
}
