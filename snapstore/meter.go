package snapstore

import (
	"context"
	"io"
	"sync/atomic"
)

// Meter counts the object bytes a store moved through this process for the calls made on one context: the bytes an
// Open's reader gave (and a Verify's read, which is one) and the bytes a Put stored. A store's server-side work (a
// HEAD, a copy inside the bucket) moves none. herd-api logs what a suspend's check and a tag moved by it
// (DEFAUL-1610).
type Meter struct{ n atomic.Int64 }

type meterKey struct{}

// WithMeter returns a context whose store calls count on the returned Meter, as well as on any Meter ctx has.
func WithMeter(ctx context.Context) (context.Context, *Meter) {
	m := &Meter{}
	return context.WithValue(ctx, meterKey{}, append(meters(ctx), m)), m
}

// Bytes is how many object bytes have moved so far.
func (m *Meter) Bytes() int64 { return m.n.Load() }

func meters(ctx context.Context) []*Meter {
	ms, _ := ctx.Value(meterKey{}).([]*Meter)
	return ms[:len(ms):len(ms)]
}

// Count adds n object bytes to ctx's meters. Stores call it for what a Put stored.
func Count(ctx context.Context, n int64) {
	for _, m := range meters(ctx) {
		m.n.Add(n)
	}
}

// CountReads counts on ctx's meters the bytes read from rc. Stores wrap what Open returns in it.
func CountReads(ctx context.Context, rc io.ReadCloser) io.ReadCloser {
	ms := meters(ctx)
	if len(ms) == 0 {
		return rc
	}
	return &countingReader{rc: rc, ms: ms}
}

type countingReader struct {
	rc io.ReadCloser
	ms []*Meter
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	for _, m := range c.ms {
		m.n.Add(int64(n))
	}
	return n, err
}

func (c *countingReader) Close() error { return c.rc.Close() }
