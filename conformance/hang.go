package conformance

import (
	"context"
	"io"
	"sync"

	"github.com/everdict/herd/snapstore"
)

// hangStore is a snapshot store that can stop answering, as an S3 endpoint frozen with docker pause does: while it
// hangs, every call waits until the store answers again or the caller's context ends (an HTTP client's request ends
// at its context, PLT-375).
type hangStore struct {
	snapstore.Store

	mu      sync.Mutex
	resumed chan struct{} // nil: answering; closed when a hang ends
}

// HangSnapshots makes the snapshot store herd-api and the herdlets share stop answering (on) or answer again (off).
func (e *Env) HangSnapshots(on bool) {
	h := e.hang
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case on && h.resumed == nil:
		h.resumed = make(chan struct{})
	case !on && h.resumed != nil:
		close(h.resumed)
		h.resumed = nil
	}
}

func (h *hangStore) wait(ctx context.Context) error {
	h.mu.Lock()
	resumed := h.resumed
	h.mu.Unlock()
	if resumed == nil {
		return nil
	}
	select {
	case <-resumed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hangStore) Put(ctx context.Context, key string, r io.Reader) (snapstore.Object, error) {
	if err := h.wait(ctx); err != nil {
		return snapstore.Object{}, err
	}
	return h.Store.Put(ctx, key, r)
}

func (h *hangStore) Stat(ctx context.Context, key string) (snapstore.Object, error) {
	if err := h.wait(ctx); err != nil {
		return snapstore.Object{}, err
	}
	return h.Store.Stat(ctx, key)
}

func (h *hangStore) Verify(ctx context.Context, key, digest string) (snapstore.Object, error) {
	if err := h.wait(ctx); err != nil {
		return snapstore.Object{}, err
	}
	return h.Store.Verify(ctx, key, digest)
}

func (h *hangStore) Prove(ctx context.Context, key, digest string) (snapstore.Object, error) {
	if err := h.wait(ctx); err != nil {
		return snapstore.Object{}, err
	}
	return h.Store.Prove(ctx, key, digest)
}

func (h *hangStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := h.wait(ctx); err != nil {
		return nil, err
	}
	return h.Store.Open(ctx, key)
}

func (h *hangStore) Delete(ctx context.Context, key string) error {
	if err := h.wait(ctx); err != nil {
		return err
	}
	return h.Store.Delete(ctx, key)
}

func (h *hangStore) Copy(ctx context.Context, from, to, digest string) (snapstore.Object, error) {
	if err := h.wait(ctx); err != nil {
		return snapstore.Object{}, err
	}
	return h.Store.Copy(ctx, from, to, digest)
}
