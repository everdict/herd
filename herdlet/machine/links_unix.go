//go:build unix

package machine

import (
	"io/fs"
	"syscall"
)

// fileID names the file fi is a name of, when it has more than one name (hard links): the same file under another
// name has the same id.
func fileID(fi fs.FileInfo) (id [2]uint64, linked bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return id, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, true //nolint:unconvert // Dev's type differs by platform
}
