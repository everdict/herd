package initd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/guest/oci"
)

// TrustBundles are the distributions' CA bundle paths: every one an image has gets herd-egress's CA appended.
var TrustBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/ssl/cert.pem",                  // Alpine, LibreSSL
}

// EgressCAPath is where every container also finds herd-egress's CA alone.
const EgressCAPath = "/etc/ssl/certs/herd-egress-ca.pem"

// maxBundle bounds the image bundle read into memory.
const maxBundle = 16 << 20

func netDir(l Layout) string { return filepath.Join(l.RunDir, "net") }

// WriteNetFiles writes the guest-wide files the containers are given: resolv.conf (when n has DNS servers) and
// herd-egress's CA (when n has one), in l.RunDir/net.
func WriteNetFiles(l Layout, n *Network) error {
	d := netDir(l)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return fmt.Errorf("initd: %w", err)
	}
	if len(n.DNS) > 0 {
		var b strings.Builder
		for _, s := range n.DNS {
			fmt.Fprintf(&b, "nameserver %s\n", s)
		}
		if err := os.WriteFile(filepath.Join(d, "resolv.conf"), []byte(b.String()), 0o644); err != nil {
			return fmt.Errorf("initd: %w", err)
		}
	}
	if n.CA != "" {
		if err := os.WriteFile(filepath.Join(d, "ca.pem"), []byte(lineEnded(n.CA)), 0o644); err != nil {
			return fmt.Errorf("initd: %w", err)
		}
	}
	log.Printf("herd-guest: network: resolvers %v written to %s, herd-egress CA %v", n.DNS, filepath.Join(d, "resolv.conf"), n.CA != "")
	return nil
}

func lineEnded(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// netFiles returns the files bound into the container with this rootfs and bundle dir: the guest's resolv.conf at
// /etc/resolv.conf, and for every TrustBundles path the image has, a copy of that bundle with herd-egress's CA
// appended (written into dir) over it — or the CA alone at the first path if the image has none. The image's
// bundles are found as the container will see them (symlinks resolved inside rootfs, as if chrooted to it) and read
// through os.Root, so a link in the image never reads a file of the guest.
func netFiles(l Layout, n *Network, rootfs, dir string) ([]oci.FileMount, error) {
	if n == nil {
		return nil, nil
	}
	var out []oci.FileMount
	if len(n.DNS) > 0 {
		out = append(out, oci.FileMount{Source: filepath.Join(netDir(l), "resolv.conf"), Destination: "/etc/resolv.conf"})
	}
	if n.CA == "" {
		return out, nil
	}
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return nil, fmt.Errorf("initd: %w", err)
	}
	defer root.Close()
	ca := filepath.Join(netDir(l), "ca.pem")
	at := map[string]bool{}
	for _, p := range TrustBundles {
		rel, err := resolveIn(root, p)
		if err != nil || rel == "" || at["/"+rel] {
			continue // absent (or a dangling link): nothing to extend
		}
		b, err := readRegular(root, rel)
		if errors.Is(err, errNotRegular) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("initd: trust bundle %s: %w", p, err)
		}
		src := filepath.Join(dir, "trust", fmt.Sprintf("%d.pem", len(at)))
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			return nil, fmt.Errorf("initd: %w", err)
		}
		if err := os.WriteFile(src, []byte(string(b)+"\n"+lineEnded(n.CA)), 0o644); err != nil {
			return nil, fmt.Errorf("initd: %w", err)
		}
		at["/"+rel] = true
		out = append(out, oci.FileMount{Source: src, Destination: "/" + rel})
	}
	if len(at) == 0 {
		at[TrustBundles[0]] = true
		out = append(out, oci.FileMount{Source: ca, Destination: TrustBundles[0]})
	}
	if !at[EgressCAPath] {
		out = append(out, oci.FileMount{Source: ca, Destination: EgressCAPath})
	}
	var dsts []string
	for _, f := range out {
		dsts = append(dsts, f.Destination)
	}
	log.Printf("herd-guest: network: container rootfs %s gets %v", rootfs, dsts)
	return out, nil
}

var errNotRegular = errors.New("not a regular file")

// readRegular reads rel from root if it is a regular file of at most maxBundle bytes.
func readRegular(root *os.Root, rel string) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBundle {
		return nil, fmt.Errorf("larger than %d bytes", maxBundle)
	}
	return b, nil
}

// resolveIn resolves the absolute path p the way a process chrooted to root would: symlinks are followed, an
// absolute target starts again at root and ".." stops there. It returns the path relative to root, with no symlink
// left in it ("" is root itself).
func resolveIn(root *os.Root, p string) (string, error) {
	var done []string
	todo := strings.Split(p, "/")
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		cur := strings.Join(append(done[:len(done):len(done)], c), "/")
		fi, err := root.Lstat(cur)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			done = append(done, c)
			continue
		}
		if hops++; hops > 40 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		t, err := root.Readlink(cur)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(t, "/") {
			done = nil
		}
		todo = append(strings.Split(t, "/"), todo...)
	}
	return strings.Join(done, "/"), nil
}
