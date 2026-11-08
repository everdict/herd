//go:build !linux

package layer

import (
	"archive/tar"
	"fmt"
	"os"
)

// listXattrs answers none: only a Linux guest's overlay has attributes a layer must keep.
func listXattrs(string) (map[string]string, error) { return nil, nil }

func setXattr(*os.Root, string, string, string) error { return nil }

func mknod(_ *os.Root, rel string, _ *tar.Header) error {
	return fmt.Errorf("%s: devices and fifos are made only in a Linux guest", rel)
}
