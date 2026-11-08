// Package initrd writes the herd guest's initramfs: a newc cpio archive of the members guest/initrd/initramfs.list
// names, in the list's order. The list is in the kernel's gen_init_cpio format (dir, nod, file, slink); a file's
// location may name an environment variable as ${NAME}, which build.sh points at the binaries built for the target
// architecture. herd builds it in Go rather than with the kernel tree's gen_init_cpio, which stamps a
// file with its source's modification time and every other member with the time of the build, so the archive is
// the same bytes wherever and whenever one revision is built (DEFAUL-1626), and testable where no kernel can be built.
package initrd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// newc mode bits for the member types the list names.
const (
	modeDir  = 0o040000
	modeReg  = 0o100000
	modeLnk  = 0o120000
	modeChr  = 0o020000
	modeBlk  = 0o060000
	trailer  = "TRAILER!!!"
	firstIno = 721 // gen_init_cpio's first inode number
)

// Member is one entry of a newc archive.
type Member struct {
	Name                 string
	Ino, Mode, UID, GID  uint32
	Nlink, MTime         uint32
	DevMajor, DevMinor   uint32
	RDevMajor, RDevMinor uint32
	Data                 []byte
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Write writes the archive list describes to w. Every member gets the modification time mtime (seconds since the
// epoch: build.sh passes SOURCE_DATE_EPOCH) and the mode and owner the list gives it, and inode numbers follow the
// list's order, so the archive is a function of the list and the files' contents alone: neither when the guest was
// built nor when its inputs were written or downloaded changes it (DEFAUL-1626).
func Write(w io.Writer, list io.Reader, getenv func(string) (string, bool), mtime int64) error {
	if mtime < 0 || mtime > math.MaxUint32 {
		return fmt.Errorf("initrd: mtime %d does not fit a newc header", mtime)
	}
	t := uint32(mtime)
	var ms []Member
	sc := bufio.NewScanner(list)
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		m, err := member(f, t, getenv)
		if err != nil {
			return fmt.Errorf("initrd: line %d: %w", n, err)
		}
		m.Ino = uint32(firstIno + len(ms))
		ms = append(ms, m)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("initrd: %w", err)
	}
	ms = append(ms, Member{Name: trailer, Nlink: 1})
	cw := &countWriter{w: bufio.NewWriter(w)}
	for _, m := range ms {
		writeMember(cw, m)
	}
	// gen_init_cpio pads the archive to a whole 512-byte block.
	if r := cw.n % 512; r != 0 {
		cw.Write(make([]byte, 512-r))
	}
	if cw.err != nil {
		return fmt.Errorf("initrd: %w", cw.err)
	}
	if err := cw.w.(*bufio.Writer).Flush(); err != nil {
		return fmt.Errorf("initrd: %w", err)
	}
	return nil
}

func member(f []string, mtime uint32, getenv func(string) (string, bool)) (Member, error) {
	want := map[string]int{"dir": 5, "nod": 8, "file": 6, "slink": 6}[f[0]]
	if want == 0 {
		return Member{}, fmt.Errorf("unknown type %q (dir, nod, file or slink)", f[0])
	}
	if len(f) != want {
		return Member{}, fmt.Errorf("%s takes %d fields, got %d", f[0], want-1, len(f)-1)
	}
	m := Member{Name: strings.TrimPrefix(f[1], "/"), Nlink: 1, MTime: mtime, DevMajor: 3, DevMinor: 1}
	var err error
	num := func(s string, base int) uint32 {
		v, e := strconv.ParseUint(s, base, 32)
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %q is not a number", f[1], s)
		}
		return uint32(v)
	}
	switch f[0] {
	case "dir":
		m.Mode, m.UID, m.GID, m.Nlink = modeDir|num(f[2], 8), num(f[3], 10), num(f[4], 10), 2
	case "nod":
		m.Mode, m.UID, m.GID = num(f[2], 8), num(f[3], 10), num(f[4], 10)
		switch f[5] {
		case "c":
			m.Mode |= modeChr
		case "b":
			m.Mode |= modeBlk
		default:
			return Member{}, fmt.Errorf("%s: device type %q (c or b)", f[1], f[5])
		}
		m.RDevMajor, m.RDevMinor = num(f[6], 10), num(f[7], 10)
	case "slink":
		m.Mode, m.UID, m.GID = modeLnk|num(f[3], 8), num(f[4], 10), num(f[5], 10)
		m.Data = append([]byte(f[2]), 0) // gen_init_cpio stores the target with its NUL
	case "file":
		m.Mode, m.UID, m.GID = modeReg|num(f[3], 8), num(f[4], 10), num(f[5], 10)
		loc, e := expand(f[2], getenv)
		if e != nil {
			return Member{}, fmt.Errorf("%s: %w", f[1], e)
		}
		if m.Data, e = os.ReadFile(loc); e != nil {
			return Member{}, fmt.Errorf("%s: %w", f[1], e)
		}
	}
	return m, err
}

func expand(s string, getenv func(string) (string, bool)) (string, error) {
	var err error
	out := envRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v, ok := getenv(name)
		if (!ok || v == "") && err == nil {
			err = fmt.Errorf("%s is not set", name)
		}
		return v
	})
	return out, err
}

func writeMember(w *countWriter, m Member) {
	fmt.Fprintf(w, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		m.Ino, m.Mode, m.UID, m.GID, m.Nlink, m.MTime, len(m.Data),
		m.DevMajor, m.DevMinor, m.RDevMajor, m.RDevMinor, len(m.Name)+1, 0)
	io.WriteString(w, m.Name)
	w.Write([]byte{0})
	w.pad()
	w.Write(m.Data)
	w.pad()
}

// Read reads the members of a newc archive up to its trailer.
func Read(r io.Reader) ([]Member, error) {
	br := &countReader{r: bufio.NewReader(r)}
	var ms []Member
	for {
		h := make([]byte, 110)
		if _, err := io.ReadFull(br, h); err != nil {
			return ms, fmt.Errorf("initrd: header %d: %w", len(ms), err)
		}
		if string(h[:6]) != "070701" {
			return ms, fmt.Errorf("initrd: header %d: magic %q", len(ms), h[:6])
		}
		var v [13]uint32
		for i := range v {
			x, err := strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 32)
			if err != nil {
				return ms, fmt.Errorf("initrd: header %d: %w", len(ms), err)
			}
			v[i] = uint32(x)
		}
		name := make([]byte, v[11])
		if _, err := io.ReadFull(br, name); err != nil {
			return ms, fmt.Errorf("initrd: name %d: %w", len(ms), err)
		}
		br.skipPad()
		data := make([]byte, v[6])
		if _, err := io.ReadFull(br, data); err != nil {
			return ms, fmt.Errorf("initrd: data %d: %w", len(ms), err)
		}
		br.skipPad()
		m := Member{Name: string(bytes.TrimRight(name, "\x00")), Ino: v[0], Mode: v[1], UID: v[2], GID: v[3], Nlink: v[4],
			MTime: v[5], DevMajor: v[7], DevMinor: v[8], RDevMajor: v[9], RDevMinor: v[10], Data: data}
		if m.Name == trailer {
			return ms, nil
		}
		ms = append(ms, m)
	}
}

type countWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
	return n, err
}

func (c *countWriter) pad() {
	if r := c.n % 4; r != 0 {
		c.Write(make([]byte, 4-r))
	}
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) skipPad() {
	if r := c.n % 4; r != 0 {
		_, _ = io.ReadFull(c, make([]byte, 4-r))
	}
}
