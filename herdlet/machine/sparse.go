package machine

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A memory file goes into a bundle as a PAX 1.0 sparse tar entry (DEFAUL-1601): its zero pages, a guest's untouched
// memory, are holes, which archive/tar's reader gives back as zeros, so every herdlet reads such a bundle as it read a
// dense one. archive/tar's writer writes no sparse entries (it drops GNU.sparse.* records), so writeSparse writes the
// entry's PAX header itself.

// zeroPage is the run of zero bytes a memory file's entry leaves out: a page.
const zeroPage = 4096

// maxFragments bounds a sparse entry's map: archive/tar reads at most 1 MiB of it, and a fragment takes two decimal
// lines of up to 12 digits.
var maxFragments = 32 << 10

// fragment is a run of a file's bytes that the entry stores.
type fragment struct{ off, n int64 }

// dataFragments lists the runs of r's size bytes that are not zero pages. If they are too many for the map, runs
// whose gap is shorter than a growing bound are merged, the zeros between them stored.
func dataFragments(r io.ReaderAt, size int64) ([]fragment, error) {
	buf := make([]byte, 1<<20)
	zero := make([]byte, zeroPage)
	var frags []fragment
	for off := int64(0); off < size; off += int64(len(buf)) {
		n := min(int64(len(buf)), size-off)
		if m, err := r.ReadAt(buf[:n], off); int64(m) != n {
			return nil, fmt.Errorf("reading at %d: %w", off, err)
		}
		for p := int64(0); p < n; p += zeroPage {
			e := min(p+zeroPage, n)
			if bytes.Equal(buf[p:e], zero[:e-p]) {
				continue
			}
			if k := len(frags); k > 0 && frags[k-1].off+frags[k-1].n == off+p {
				frags[k-1].n += e - p
			} else {
				frags = append(frags, fragment{off + p, e - p})
			}
		}
	}
	for gap := int64(zeroPage); len(frags) > maxFragments; gap *= 2 {
		merged := frags[:1]
		for _, f := range frags[1:] {
			if last := &merged[len(merged)-1]; f.off-(last.off+last.n) <= gap {
				last.n = f.off + f.n - last.off
			} else {
				merged = append(merged, f)
			}
		}
		frags = merged
	}
	return frags, nil
}

// writeSparse writes r's size bytes as entry name of tw, which writes to w, storing only frags.
func writeSparse(tw *tar.Writer, w io.Writer, name string, size int64, r io.ReaderAt, frags []fragment) error {
	if k := len(frags); k == 0 || frags[k-1].off+frags[k-1].n < size {
		// GNU's convention: a file that ends in a hole says where it ends.
		frags = append(frags, fragment{size, 0})
	}
	var m strings.Builder
	m.WriteString(strconv.Itoa(len(frags)) + "\n")
	var data int64
	for _, f := range frags {
		m.WriteString(strconv.FormatInt(f.off, 10) + "\n" + strconv.FormatInt(f.n, 10) + "\n")
		data += f.n
	}
	sparseMap := []byte(m.String())
	sparseMap = append(sparseMap, make([]byte, (512-len(sparseMap)%512)%512)...)
	pax, err := paxHeader(map[string]string{
		"GNU.sparse.major": "1", "GNU.sparse.minor": "0", "GNU.sparse.name": name, "GNU.sparse.realsize": strconv.FormatInt(size, 10),
	})
	if err != nil {
		return err
	}
	// The entry before ends (padded) before the PAX header goes out, and the entry it describes follows it at once.
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := w.Write(pax); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: bundleMemory + "/GNUSparseFile.0/m", Mode: 0o600, Size: int64(len(sparseMap)) + data,
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	if _, err := tw.Write(sparseMap); err != nil {
		return err
	}
	for _, f := range frags {
		if _, err := io.Copy(tw, io.NewSectionReader(r, f.off, f.n)); err != nil {
			return err
		}
	}
	return nil
}

// paxHeader is a PAX extended header (typeflag 'x') holding the records, padded to its last block.
func paxHeader(recs map[string]string) ([]byte, error) {
	var body strings.Builder
	for _, k := range []string{"GNU.sparse.major", "GNU.sparse.minor", "GNU.sparse.name", "GNU.sparse.realsize"} {
		v, ok := recs[k]
		if !ok {
			continue
		}
		// "<length> <key>=<value>\n", the length counting itself.
		rest := " " + k + "=" + v + "\n"
		n := len(rest) + 1
		for len(strconv.Itoa(n))+len(rest) != n {
			n = len(strconv.Itoa(n)) + len(rest)
		}
		body.WriteString(strconv.Itoa(n) + rest)
	}
	// archive/tar writes the block as a regular file's; its type and checksum are then the PAX header's.
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: bundleMemory + "/PaxHeaders.0/m", Mode: 0o644, Size: int64(body.Len()),
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(body.String())); err != nil {
		return nil, err
	}
	if err := tw.Flush(); err != nil {
		return nil, err
	}
	out := b.Bytes()
	if len(out) < 512 {
		return nil, errors.New("machine: bundle: no PAX header block")
	}
	out[156] = tar.TypeXHeader
	copy(out[148:156], "        ")
	var sum int64
	for _, c := range out[:512] {
		sum += int64(c)
	}
	copy(out[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return out, nil
}
