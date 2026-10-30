// Command vzguest is the /init of the guest vmm/vz's real-host test boots (HERD_VZ_REAL=1, see realtest.sh). It
// mounts the virtio-fs share (tag herd) at /mnt/volumes and serves one line-based request per vsock connection on
// port 8080:
//
//	read <name>          the contents of /mnt/volumes/<name>
//	write <name> <text>  writes <text> to /mnt/volumes/<name>, answers "ok"
//	echo                 echoes every byte after the line until the host closes its side
//
// It exists so the backend is tested against a real guest without the container machinery herd-guest needs.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	port  = 8080
	share = "/mnt/volumes"
)

func main() {
	if err := run(); err != nil {
		log.Printf("vzguest: %v", err)
		os.Exit(1)
	}
}

func serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go handle(c)
	}
}

func handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	verb, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	switch verb {
	case "read":
		b, err := os.ReadFile(filepath.Join(share, filepath.Base(rest)))
		if err != nil {
			fmt.Fprintf(c, "error: %v", err)
			return
		}
		_, _ = c.Write(b)
	case "write":
		name, text, _ := strings.Cut(rest, " ")
		if err := os.WriteFile(filepath.Join(share, filepath.Base(name)), []byte(text), 0o644); err != nil {
			fmt.Fprintf(c, "error: %v", err)
			return
		}
		fmt.Fprint(c, "ok")
	case "echo":
		_, _ = io.Copy(c, r)
	default:
		fmt.Fprintf(c, "error: unknown request %q", verb)
	}
}
