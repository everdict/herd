package oci

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// maxUserFile bounds what is read of the image's /etc/passwd and /etc/group.
const maxUserFile = 4 << 20

// ResolveUser answers the identity of an image's USER (name or uid, optionally :group or :gid) as Docker resolves it
// (moby/sys/user GetExecUser, defaults root): a name must be in the image's /etc/passwd and a group name in its
// /etc/group; a numeric uid not in /etc/passwd runs with gid 0. Without an explicit group, the user's groups in
// /etc/group are its additional gids (for an empty USER, root's). The files are read through os.Root at rootfs, so a
// symlink in the image cannot point outside it.
func ResolveUser(rootfs, spec string) (User, error) {
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return User{}, fmt.Errorf("oci: user %q: %w", spec, err)
	}
	defer root.Close()
	passwd, err := readEntries(root, "etc/passwd")
	if err != nil {
		return User{}, fmt.Errorf("oci: user %q: %w", spec, err)
	}
	group, err := readEntries(root, "etc/group")
	if err != nil {
		return User{}, fmt.Errorf("oci: user %q: %w", spec, err)
	}
	return resolveUser(spec, passwd, group)
}

// readEntries reads a colon-separated database; a missing file has no entries.
func readEntries(root *os.Root, name string) ([][]string, error) {
	f, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(&limitedReader{f, maxUserFile})
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading /%s: %w", name, err)
	}
	return out, nil
}

type limitedReader struct {
	f *os.File
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("larger than %d bytes", maxUserFile)
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.f.Read(p)
	l.n -= int64(n)
	return n, err
}

// id parses a uid or gid; ok is false for anything but a decimal number in range.
func id(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

func field(e []string, i int) string {
	if i < len(e) {
		return e[i]
	}
	return ""
}

func resolveUser(spec string, passwd, group [][]string) (User, error) {
	userArg, groupArg, _ := strings.Cut(spec, ":")
	uidArg, uidNum := id(userArg)
	gidArg, gidNum := id(groupArg)
	var u User
	matched := ""
	for _, e := range passwd {
		uid, ok1 := id(field(e, 2))
		gid, ok2 := id(field(e, 3))
		if !ok1 || !ok2 {
			continue
		}
		if userArg == "" && uid == 0 || uidNum && uid == uidArg || !uidNum && userArg != "" && field(e, 0) == userArg {
			u.UID, u.GID, matched = uid, gid, field(e, 0)
			break
		}
	}
	if matched == "" && userArg != "" {
		if !uidNum {
			return User{}, fmt.Errorf("oci: unable to find user %s: no matching entries in passwd file", userArg)
		}
		u.UID = uidArg
	}
	if groupArg == "" && matched == "" {
		return u, nil
	}
	for _, e := range group {
		gid, ok := id(field(e, 2))
		if !ok {
			continue
		}
		if groupArg != "" {
			if gidNum && gid == gidArg || !gidNum && field(e, 0) == groupArg {
				u.GID = gid
				return u, nil
			}
			continue
		}
		for _, m := range strings.Split(field(e, 3), ",") {
			if strings.TrimSpace(m) == matched {
				u.AdditionalGids = append(u.AdditionalGids, gid)
				break
			}
		}
	}
	if groupArg != "" {
		if !gidNum {
			return User{}, fmt.Errorf("oci: unable to find group %s: no matching entries in group file", groupArg)
		}
		u.GID = gidArg
	}
	return u, nil
}
