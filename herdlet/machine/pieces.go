package machine

import (
	"archive/tar"
	"fmt"
	"io"
	"maps"
	"strconv"

	"github.com/pierrec/lz4/v4"
)

// From bundle version 5 a volume's file and a container's layer archive go into a bundle in pieces (PLT-591), so a
// workspace costs the bytes LZ4 leaves of it, not its raw size: a checkout with its node_modules is most of a
// delegate box's bundle, and text. A file is one tar entry per pieceRaw raw bytes, all under the file's name and in
// order:
//
//   - the first entry is the file's own (its mode, owner and time); an empty file is that entry alone;
//   - each further entry carries the PAX record paxPart, 1, 2, ... in order;
//   - an entry whose bytes are an LZ4 block (pierrec/lz4) carries paxRaw, the raw length they expand to; an entry
//     without it holds its raw bytes (LZ4 did not make them shorter).
//
// A reader appends the pieces as they come and refuses a piece out of order, one that expands to other than its raw
// length, and a raw length over pieceRaw. Bundles before version 5 have neither record, and read as they always did.
const (
	pieceRaw = frameRaw
	paxRaw   = "HERD.raw"
	paxPart  = "HERD.part"
)

// pieces compresses the files of one bundle: one LZ4 compressor and its two buffers, used for a file at a time.
type pieces struct {
	c   lz4.Compressor
	raw []byte
	buf []byte
}

func newPieces() *pieces {
	return &pieces{raw: make([]byte, pieceRaw), buf: make([]byte, lz4.CompressBlockBound(pieceRaw))}
}

// write writes exactly size bytes of r to tw as the pieces of the file h describes. h is not changed.
func (p *pieces) write(tw *tar.Writer, h *tar.Header, r io.Reader, size int64) error {
	for part, done := 0, int64(0); part == 0 || done < size; part++ {
		raw := p.raw[:min(int64(pieceRaw), size-done)]
		if _, err := io.ReadFull(r, raw); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("reading at %d of %d: %w", done, size, err)
		}
		stored, err := encodeFrame(&p.c, p.buf, raw)
		if err != nil {
			return err
		}
		e := *h
		e.Size = int64(len(stored))
		e.PAXRecords = maps.Clone(h.PAXRecords)
		if len(stored) < len(raw) || part > 0 {
			if e.PAXRecords == nil {
				e.PAXRecords = map[string]string{}
			}
			if len(stored) < len(raw) {
				e.PAXRecords[paxRaw] = strconv.Itoa(len(raw))
			}
			if part > 0 {
				e.PAXRecords[paxPart] = strconv.Itoa(part)
			}
		}
		if err := tw.WriteHeader(&e); err != nil {
			return err
		}
		if _, err := tw.Write(stored); err != nil {
			return err
		}
		done += int64(len(raw))
	}
	return nil
}

// partOf is which piece of its file the entry h is: 0 for the first (and for every entry of a bundle before
// version 5).
func partOf(h *tar.Header) (int, error) {
	v, ok := h.PAXRecords[paxPart]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("machine: bundle: %s: piece %q is no piece number", h.Name, v)
	}
	return n, nil
}

// readPiece writes the raw bytes of the entry h, whose stored bytes r gives, to w.
func (p *pieces) readPiece(r io.Reader, h *tar.Header, w io.Writer) error {
	v, ok := h.PAXRecords[paxRaw]
	if !ok {
		if _, err := io.CopyN(w, r, h.Size); err != nil {
			return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
		}
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > pieceRaw || h.Size < 1 || h.Size >= int64(n) {
		return fmt.Errorf("machine: bundle: %s: a piece of %d stored bytes cannot be %q raw bytes", h.Name, h.Size, v)
	}
	stored := p.buf[:h.Size]
	if _, err := io.ReadFull(r, stored); err != nil {
		return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
	}
	raw := p.raw[:n]
	got, err := lz4.UncompressBlock(stored, raw)
	if err != nil {
		return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
	}
	if got != n {
		return fmt.Errorf("machine: bundle: %s: a piece expands to %d bytes, not its %d", h.Name, got, n)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("machine: bundle: %s: %w", h.Name, err)
	}
	return nil
}
