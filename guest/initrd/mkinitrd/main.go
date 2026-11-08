// Command mkinitrd writes the herd guest's initramfs (an uncompressed newc cpio) for a gen_init_cpio list to
// standard output, every member with modification time <mtime> (seconds since the epoch); guest/build.sh passes
// SOURCE_DATE_EPOCH and compresses it into initrd.img.
//
//	go run ./guest/initrd/mkinitrd <mtime> <list>
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/everdict/herd/guest/initrd"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkinitrd <mtime> <list>")
		os.Exit(2)
	}
	mtime, err := strconv.ParseInt(os.Args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkinitrd: mtime %q is not seconds since the epoch\n", os.Args[1])
		os.Exit(2)
	}
	f, err := os.Open(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkinitrd:", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := initrd.Write(os.Stdout, f, os.LookupEnv, mtime); err != nil {
		fmt.Fprintln(os.Stderr, "mkinitrd:", err)
		os.Exit(1)
	}
}
