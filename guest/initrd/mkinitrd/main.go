// Command mkinitrd writes the herd guest's initramfs (an uncompressed newc cpio) for a gen_init_cpio list to
// standard output; guest/build.sh compresses it into initrd.img.
//
//	go run ./guest/initrd/mkinitrd <list>
package main

import (
	"fmt"
	"os"

	"github.com/everdict/herd/guest/initrd"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkinitrd <list>")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkinitrd:", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := initrd.Write(os.Stdout, f, os.LookupEnv); err != nil {
		fmt.Fprintln(os.Stderr, "mkinitrd:", err)
		os.Exit(1)
	}
}
