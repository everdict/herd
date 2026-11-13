package snapstore

import (
	"crypto/sha256"
	"hash"
	"io"
	"os"
)

// DefaultPartSize is the part size a store uploads a large object in unless it says otherwise (PartSizer), and so the
// part size a Summer sums at by default.
const DefaultPartSize = 64 << 20

// Summed is a source of a Put that already knows its own sums, made as it was written (PLT-591): a store that would
// hash the source before sending it takes them instead and reads the source once. Sums answers the source's size, the
// SHA-256 of all of it and the SHA-256 of each consecutive part bytes of it (the last may be shorter); ok is false
// when it did not sum at that part size. A store that takes them still has the endpoint refuse bytes that are not
// theirs, so a source that changed after its sums is refused, not stored.
type Summed interface {
	Sums(part int64) (size int64, sum []byte, parts [][]byte, ok bool)
}

// PartSizer is a store that uploads a large object in parts of PartSize bytes: what a Summed source must sum at.
type PartSizer interface {
	SumPartSize() int64
}

// PartSizeOf is the part size s uploads in: its own (PartSizer), else DefaultPartSize.
func PartSizeOf(s Store) int64 {
	if p, ok := s.(PartSizer); ok {
		if n := p.SumPartSize(); n > 0 {
			return n
		}
	}
	return DefaultPartSize
}

// Summer is the sums of what is written to it, at one part size: a Summed source's.
type Summer struct {
	part  int64
	n     int64
	whole hash.Hash
	cur   hash.Hash
	curN  int64
	parts [][]byte
}

// NewSummer sums at part bytes per part (zero or less: DefaultPartSize).
func NewSummer(part int64) *Summer {
	if part <= 0 {
		part = DefaultPartSize
	}
	return &Summer{part: part, whole: sha256.New()}
}

func (s *Summer) Write(b []byte) (int, error) {
	s.whole.Write(b)
	s.n += int64(len(b))
	total := len(b)
	for len(b) > 0 {
		if s.cur == nil {
			s.cur, s.curN = sha256.New(), 0
		}
		k := min(int64(len(b)), s.part-s.curN)
		s.cur.Write(b[:k])
		s.curN += k
		b = b[k:]
		if s.curN == s.part {
			s.parts, s.cur = append(s.parts, s.cur.Sum(nil)), nil
		}
	}
	return total, nil
}

// Size is how many bytes were written.
func (s *Summer) Size() int64 { return s.n }

// Sums answers what was written so far; ok only at the part size it sums at.
func (s *Summer) Sums(part int64) (int64, []byte, [][]byte, bool) {
	if part != s.part {
		return 0, nil, nil, false
	}
	parts := s.parts[:len(s.parts):len(s.parts)]
	if s.cur != nil {
		parts = append(parts, s.cur.Sum(nil))
	}
	return s.n, s.whole.Sum(nil), parts, true
}

// SummedFile is f, read from its start, with the sums of its bytes that s made as they were written.
func SummedFile(f *os.File, s *Summer) io.ReadSeeker { return summedFile{f, s} }

type summedFile struct {
	*os.File
	s *Summer
}

func (f summedFile) Sums(part int64) (int64, []byte, [][]byte, bool) { return f.s.Sums(part) }
