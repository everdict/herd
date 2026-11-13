package machine

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

// TestMeasureFrameCodec (PLT-591) compares, on bytes like a guest's used memory, the two codecs a memory frame could
// use: DEFLATE at BestSpeed (round 1) and LZ4's block format. The corpus is the Go toolchain's files and the build
// cache (executables, object files, source: what a guest's page cache and heap hold), read in 4 MiB frames with their
// zero pages left out as the bundle does, and each frame is compressed and expanded on min(GOMAXPROCS, 4) cores as
// the bundle does. It runs only when HERD_MEASURE is set (HERD_MEASURE_MIB, default 256). Lines start with MEASURE.
func TestMeasureFrameCodec(t *testing.T) {
	if os.Getenv("HERD_MEASURE") == "" {
		t.Skip("HERD_MEASURE is not set")
	}
	limit := int64(256) << 20
	if v := os.Getenv("HERD_MEASURE_MIB"); v != "" {
		var n int64
		for _, c := range v {
			n = n*10 + int64(c-'0')
		}
		limit = n << 20
	}
	var corpus bytes.Buffer
	roots := []string{runtime.GOROOT()}
	if c, err := os.UserCacheDir(); err == nil {
		roots = append(roots, filepath.Join(c, "go-build"))
	}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || int64(corpus.Len()) >= limit {
				return nil
			}
			b, err := os.ReadFile(p)
			if err == nil {
				corpus.Write(b[:min(int64(len(b)), limit-int64(corpus.Len()))])
			}
			return nil
		})
	}
	var frames []*frame
	if err := splitFrames(bytes.NewReader(corpus.Bytes()), int64(corpus.Len()), sha256.New(), func(fr *frame) bool { frames = append(frames, fr); return true }); err != nil {
		t.Fatal(err)
	}
	var raw int64
	for _, fr := range frames {
		raw += int64(len(fr.raw))
	}
	t.Logf("MEASURE corpus_bytes=%d frame_raw_bytes=%d frames=%d workers=%d", corpus.Len(), raw, len(frames), min(runtime.GOMAXPROCS(0), 4))
	type codec struct {
		name       string
		compress   func(dst *bytes.Buffer, src []byte)
		decompress func(src []byte, n int) []byte
	}
	codecs := []codec{
		{"deflate-bestspeed", func(dst *bytes.Buffer, src []byte) {
			zw, _ := flate.NewWriter(dst, flate.BestSpeed)
			zw.Write(src)
			zw.Close()
		}, func(src []byte, n int) []byte {
			b := make([]byte, n)
			io.ReadFull(flate.NewReader(bytes.NewReader(src)), b)
			return b
		}},
		{"lz4-block", func(dst *bytes.Buffer, src []byte) {
			var c lz4.Compressor
			out := make([]byte, lz4.CompressBlockBound(len(src)))
			n, _ := c.CompressBlock(src, out)
			dst.Write(out[:n])
		}, func(src []byte, n int) []byte {
			b := make([]byte, n)
			lz4.UncompressBlock(src, b)
			return b
		}},
	}
	for _, c := range codecs {
		outs := make([]bytes.Buffer, len(frames))
		start := time.Now()
		each(len(frames), func(i int) { c.compress(&outs[i], frames[i].raw) })
		ct := time.Since(start)
		var stored int64
		for i := range outs {
			stored += int64(outs[i].Len())
		}
		start = time.Now()
		each(len(frames), func(i int) {
			if !bytes.Equal(c.decompress(outs[i].Bytes(), len(frames[i].raw)), frames[i].raw) {
				t.Errorf("%s: frame %d does not expand to its bytes", c.name, i)
			}
		})
		dt := time.Since(start)
		t.Logf("MEASURE codec=%s stored_bytes=%d ratio=%.3f compress_s=%.3f compress_MiBps=%.0f expand_s=%.3f expand_MiBps=%.0f",
			c.name, stored, float64(stored)/float64(raw), ct.Seconds(), float64(raw)/(1<<20)/ct.Seconds(), dt.Seconds(), float64(raw)/(1<<20)/dt.Seconds())
	}
}

// each runs f for 0..n-1 on min(GOMAXPROCS, 4) goroutines, as writeMemoryFrames does.
func each(n int, f func(int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(runtime.GOMAXPROCS(0), 4) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}
