package machine

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"runtime"

	"github.com/everdict/herd/vmm"
	"github.com/pierrec/lz4/v4"
)

// From bundle version 4 a memory file goes into a bundle as frames (PLT-591), so a VM's memory costs the bytes it
// holds, not the bytes of its limit:
//
//   - memory/<name>/<n>, n = 00000000, 00000001, ...: one frame, a 16-byte header (the raw offset and the raw length,
//     big-endian) and then those raw bytes as one LZ4 block (pierrec/lz4), or the raw bytes themselves where LZ4 does not make them
//     shorter: a frame whose stored bytes are as long as its raw length holds them raw.
//     A frame holds at most frameRaw raw bytes; frames are in offset order and do not overlap.
//   - memory/<name>/end: the raw size and the SHA-256 of all raw bytes, 8 + 32 bytes.
//
// Raw bytes no frame covers are zeros: a run of zero pages of at least zeroGap bytes is in no frame, and a shorter one
// is compressed with its neighbours. A reader expands the frames and checks the size and the digest before it hands
// over the file's last byte, so a restore never runs on memory other than the snapshot's.
// zeroPage is the run of zero bytes a memory file's frames leave out: a page.
const zeroPage = 4096

const (
	frameRaw    = 4 << 20
	zeroGap     = 64 << 10
	frameHeader = 16
	frameEnd    = "end"
	endSize     = 8 + sha256.Size
)

// frame is one frame's raw bytes at its offset and, once compressed, its stored bytes.
type frame struct {
	off    int64
	raw    []byte
	out    bytes.Buffer
	err    error
	stored []byte // the last frame's stored bytes
	done   chan struct{}
}

// frameName is the tar entry of the memory file's frame n.
func frameName(file string, n int) string { return fmt.Sprintf("%s/%s/%08d", bundleMemory, file, n) }

// writeMemoryFrames writes one of the VMM's snapshot files as frames to tw: it is read once, in order, hashed, its
// zero pages left out, and the rest compressed on up to four cores.
func writeMemoryFrames(tw *tar.Writer, f vmm.SnapshotFile) error {
	if f.Open == nil {
		return errors.New("no way to read it")
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	workers := min(runtime.GOMAXPROCS(0), 4)
	order := make(chan *frame, 2*workers)
	work := make(chan *frame, 2*workers)
	stop := make(chan struct{})
	for range workers {
		go func() {
			var c lz4.Compressor
			buf := make([]byte, lz4.CompressBlockBound(frameRaw))
			for fr := range work {
				var hdr [frameHeader]byte
				binary.BigEndian.PutUint64(hdr[:8], uint64(fr.off))
				binary.BigEndian.PutUint64(hdr[8:], uint64(len(fr.raw)))
				fr.out.Write(hdr[:])
				var b []byte
				b, fr.err = encodeFrame(&c, buf, fr.raw)
				fr.out.Write(b)
				fr.raw = nil
				close(fr.done)
			}
		}()
	}
	h := sha256.New()
	var readErr error
	go func() {
		defer close(order)
		defer close(work)
		readErr = splitFrames(r, f.Size, h, func(fr *frame) bool {
			select {
			case order <- fr:
			case <-stop:
				return false
			}
			work <- fr
			return true
		})
	}()
	n := 0
	var werr error
	for fr := range order {
		<-fr.done
		if werr != nil {
			continue
		}
		if werr = fr.err; werr == nil {
			werr = writeEntry(tw, frameName(f.Name, n), fr.out.Bytes())
		}
		if werr != nil {
			close(stop)
		}
		n++
	}
	if werr != nil {
		return werr
	}
	if readErr != nil {
		return readErr
	}
	end := make([]byte, 0, endSize)
	end = binary.BigEndian.AppendUint64(end, uint64(f.Size))
	return writeEntry(tw, bundleMemory+"/"+f.Name+"/"+frameEnd, h.Sum(end))
}

// splitFrames reads size bytes of r in order into h and hands each frame of them that is not zero pages to emit,
// until emit answers false.
func splitFrames(r io.Reader, size int64, h hash.Hash, emit func(*frame) bool) error {
	zero := make([]byte, zeroPage)
	for off := int64(0); off < size; {
		buf := make([]byte, min(frameRaw, size-off))
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("reading at %d: %w", off, err)
		}
		h.Write(buf)
		// The runs of the block that are not zero pages, joined across gaps shorter than zeroGap.
		start, end := -1, -1
		flush := func() bool {
			if start < 0 {
				return true
			}
			fr := &frame{off: off + int64(start), raw: buf[start:end], done: make(chan struct{})}
			start, end = -1, -1
			return emit(fr)
		}
		for p := 0; p < len(buf); p += zeroPage {
			e := min(p+zeroPage, len(buf))
			if bytes.Equal(buf[p:e], zero[:e-p]) {
				continue
			}
			if start >= 0 && p-end >= zeroGap && !flush() {
				return nil
			}
			if start < 0 {
				start = p
			}
			end = e
		}
		if !flush() {
			return nil
		}
		off += int64(len(buf))
	}
	return nil
}

// encodeFrame is raw as a frame stores it: an LZ4 block, compressed with c into buf (at least
// lz4.CompressBlockBound(len(raw)) bytes), or raw itself when LZ4 does not make it shorter.
func encodeFrame(c *lz4.Compressor, buf, raw []byte) ([]byte, error) {
	n, err := c.CompressBlock(raw, buf)
	if err != nil {
		return nil, err
	}
	if n == 0 || n >= len(raw) {
		return raw, nil
	}
	return buf[:n], nil
}

// decodeFrame expands a frame's stored bytes into raw, which is exactly the frame's raw length: it fails on stored
// bytes that expand to more or fewer.
func decodeFrame(stored, raw []byte) error {
	if len(stored) == len(raw) {
		copy(raw, stored)
		return nil
	}
	n, err := lz4.UncompressBlock(stored, raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return fmt.Errorf("expands to %d bytes", n)
	}
	return nil
}

func writeEntry(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// frameReader gives back a memory file stored as frames: its raw bytes, in order, exactly its size of them. It fails
// on a frame out of order or out of the file, a frame that expands to other than its raw length, and a file whose
// size or digest is not its end entry's; it reads the end entry and checks it before it gives the last byte.
type frameReader struct {
	tr     *tar.Reader
	file   vmm.SnapshotFile
	h      hash.Hash
	pos    int64 // raw bytes produced (and hashed) so far
	out    []byte
	n      int // frames read
	next   *frameHead
	end    []byte // the end entry, once read
	done   bool   // the end entry was read
	err    error
	stored []byte // the last frame's stored bytes, reused
}

type frameHead struct{ off, n, stored int64 }

var zeros = make([]byte, 1<<20)

func newFrameReader(tr *tar.Reader, f vmm.SnapshotFile) *frameReader {
	return &frameReader{tr: tr, file: f, h: sha256.New()}
}

func (r *frameReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.pos == r.file.Size {
			return 0, io.EOF
		}
		if r.err = r.produce(); r.err != nil {
			r.out = nil
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// produce makes the next raw bytes: zeros up to the next frame (or the end), or the next frame expanded. The last
// raw bytes are produced only once the end entry agrees with all of them.
func (r *frameReader) produce() error {
	if r.next == nil && !r.done {
		if err := r.entry(); err != nil {
			return err
		}
	}
	upTo := r.file.Size
	if r.next != nil {
		upTo = r.next.off
	}
	if r.pos < upTo {
		r.out = zeros[:min(int64(len(zeros)), upTo-r.pos)]
	} else {
		b, err := r.expand(*r.next)
		if err != nil {
			return err
		}
		r.out, r.next = b, nil
	}
	r.h.Write(r.out)
	r.pos += int64(len(r.out))
	if r.pos == r.file.Size {
		if !r.done {
			if err := r.entry(); err != nil {
				return err
			}
			if !r.done {
				return fmt.Errorf("machine: bundle: memory file %s: a frame at %d past its %d bytes", r.file.Name, r.next.off, r.file.Size)
			}
		}
		if sum := r.h.Sum(nil); binary.BigEndian.Uint64(r.end[:8]) != uint64(r.file.Size) || !bytes.Equal(sum, r.end[8:]) {
			return fmt.Errorf("machine: bundle: memory file %s expands to %d bytes sha256:%x, its end entry records %d bytes sha256:%x",
				r.file.Name, r.file.Size, sum, binary.BigEndian.Uint64(r.end[:8]), r.end[8:])
		}
	}
	return nil
}

// entry reads the next entry of the file: a frame's header into next, or the end entry.
func (r *frameReader) entry() error {
	hd, err := r.tr.Next()
	if err != nil {
		return fmt.Errorf("machine: bundle: memory file %s: %w", r.file.Name, err)
	}
	if hd.Typeflag != tar.TypeReg {
		return fmt.Errorf("machine: bundle: entry %q where memory file %s's frames belong", hd.Name, r.file.Name)
	}
	if hd.Name == bundleMemory+"/"+r.file.Name+"/"+frameEnd {
		if hd.Size != endSize {
			return fmt.Errorf("machine: bundle: memory file %s: an end entry of %d bytes", r.file.Name, hd.Size)
		}
		r.end = make([]byte, endSize)
		if _, err := io.ReadFull(r.tr, r.end); err != nil {
			return fmt.Errorf("machine: bundle: memory file %s: %w", r.file.Name, err)
		}
		r.done = true
		return nil
	}
	if hd.Name != frameName(r.file.Name, r.n) || hd.Size < frameHeader || hd.Size > frameHeader+int64(lz4.CompressBlockBound(frameRaw)) {
		return fmt.Errorf("machine: bundle: entry %q (%d bytes) where frame %d of memory file %s belongs", hd.Name, hd.Size, r.n, r.file.Name)
	}
	var b [frameHeader]byte
	if _, err := io.ReadFull(r.tr, b[:]); err != nil {
		return fmt.Errorf("machine: bundle: memory file %s: %w", r.file.Name, err)
	}
	fh := frameHead{off: int64(binary.BigEndian.Uint64(b[:8])), n: int64(binary.BigEndian.Uint64(b[8:])), stored: hd.Size - frameHeader}
	if fh.off < r.pos || fh.n <= 0 || fh.n > frameRaw || fh.off > r.file.Size-fh.n {
		return fmt.Errorf("machine: bundle: memory file %s: frame %d holds %d bytes at %d, after %d of %d bytes", r.file.Name, r.n, fh.n, fh.off, r.pos, r.file.Size)
	}
	r.n++
	r.next = &fh
	return nil
}

// expand reads the current frame's stored bytes and expands them: exactly its raw length of them.
func (r *frameReader) expand(fh frameHead) ([]byte, error) {
	if cap(r.stored) < int(fh.stored) {
		r.stored = make([]byte, fh.stored)
	}
	stored := r.stored[:fh.stored]
	if _, err := io.ReadFull(r.tr, stored); err != nil {
		return nil, fmt.Errorf("machine: bundle: memory file %s: frame at %d: %w", r.file.Name, fh.off, err)
	}
	b := make([]byte, fh.n)
	if err := decodeFrame(stored, b); err != nil {
		return nil, fmt.Errorf("machine: bundle: memory file %s: frame at %d does not expand to its %d bytes: %w", r.file.Name, fh.off, fh.n, err)
	}
	return b, nil
}

// skip reads past the rest of the file's entries, through its end entry, without expanding them.
func (r *frameReader) skip() error {
	for !r.done {
		if r.err != nil {
			return r.err
		}
		r.next = nil
		if err := r.entry(); err != nil {
			return err
		}
	}
	return nil
}
