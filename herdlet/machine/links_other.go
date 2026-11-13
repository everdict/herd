//go:build !unix

package machine

import "io/fs"

// fileID knows no hard links here: every name is written as its own file.
func fileID(fs.FileInfo) (id [2]uint64, linked bool) { return id, false }
