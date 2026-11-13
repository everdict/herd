package image_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// slowBuilder is a filesystem tool that takes as long as the test says: Build waits for release, or ends with its
// context's error when that is cancelled first, as mke2fs run under a deadline does.
type slowBuilder struct {
	*imagetest.Builder
	started chan struct{}
	release chan struct{}
	begun   atomic.Int32
}

func (b *slowBuilder) Build(ctx context.Context, tarPath, outPath string) error {
	b.begun.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Builder.Build(ctx, tarPath, outPath)
}

// TestBuildOutlivesItsCaller is the counterexample of a boot that never converges (PLT-591): an image whose
// filesystem takes longer to build than one caller waits. The caller that asked first gives up at its deadline; the
// build goes on, the next caller joins it, and the image is built once. A build that ends with its first caller is
// started again by every retry and never ends: on one host mke2fs for a 1 GB image needed more than the three
// minutes a boot waits, and the template's golden was attempted 100 times in five hours.
func TestBuildOutlivesItsCaller(t *testing.T) {
	reg := imagetest.NewRegistry()
	b := &slowBuilder{Builder: &imagetest.Builder{}, started: make(chan struct{}, 1), release: make(chan struct{})}
	c := &image.Cache{Dir: t.TempDir(), Source: reg, Builder: b, Platform: amd64}
	reg.Push("team/tool", "v1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "version", Body: "one"})}})
	const ref = "registry.example/team/tool:v1"

	first, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := c.Get(first, ref)
		firstErr <- err
	}()
	<-b.started
	cancel()
	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the caller that gave up was answered %v, want its context's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the caller that gave up was not answered: it waits for the build past its own deadline")
	}

	second := make(chan error, 1)
	var got image.Rootfs
	go func() {
		r, err := c.Get(context.Background(), ref)
		got = r
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("the second caller was answered (%v) before the filesystem tool ended", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(b.release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the second caller: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second caller was not answered after the build ended")
	}
	if read(t, got.Path) == "" {
		t.Fatal("the image is empty")
	}
	if n := b.begun.Load(); n != 1 {
		t.Fatalf("the filesystem tool was started %d times, want once: the build ended with its first caller and the retry began it again", n)
	}
}

// TestBuildHasItsOwnBound: a build no caller waits for still ends, at BuildTimeout, and the next Get builds again.
func TestBuildHasItsOwnBound(t *testing.T) {
	reg := imagetest.NewRegistry()
	b := &slowBuilder{Builder: &imagetest.Builder{}, started: make(chan struct{}, 1), release: make(chan struct{})}
	c := &image.Cache{Dir: t.TempDir(), Source: reg, Builder: b, Platform: amd64, BuildTimeout: 50 * time.Millisecond}
	reg.Push("team/tool", "v1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "version", Body: "one"})}})
	const ref = "registry.example/team/tool:v1"
	if _, err := c.Get(context.Background(), ref); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a build past BuildTimeout answered %v, want a deadline error", err)
	}
	close(b.release)
	if _, err := c.Get(context.Background(), ref); err != nil {
		t.Fatalf("the Get after a build that ran out of time: %v", err)
	}
}
