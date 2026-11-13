package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/mkfs"
	"github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/bundlecache"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/cloudhypervisor"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// Herdlet is a herdlet's configuration (ADR A-19, A-20): `herdlet join <herd-api> --token <t>`, `herdlet run` (or
// bare `herdlet`) and `herdlet doctor`. It turns actor templates into microVMs on the host's hypervisor — Cloud
// Hypervisor on Linux, Virtualization.framework on a Mac — with rootfs images built from the templates' OCI images
// (image cache under -state), DurableDir volumes in each VM's virtio-fs share, and warm VMs per template.
type Herdlet struct {
	// API is herd-api's address (join: the positional argument; run: -api with -token-file).
	API string
	// Token and TokenFile are the join token (join only).
	Token     string
	TokenFile string
	// CredentialFile is a herdlet credential from `herd-api token -herdlet` (run only): with it the herdlet does not
	// join and dials -api as -worker.
	CredentialFile string
	Worker         string
	StateDir       string
	Snapshots      string
	// BundleCacheMiB bounds <state>/bundles, the node's copy of the snapshot objects it wrote to or read from an S3
	// store (-bundle-cache, PLT-227): a restore of one it holds reads no S3 bytes. 0 keeps none.
	BundleCacheMiB int
	// ImageCacheMiB bounds <state>/images, the rootfs images and layer blobs built here (-image-cache; 0: no bound):
	// past it, the least recently used that no VM runs on are removed (A-58).
	ImageCacheMiB int
	Slots         int
	// ReserveCPU (millicores) and ReserveMemoryMiB are what the host keeps for itself, and CPUOvercommit how many vCPUs
	// a core carries: what remains is what the herdlet reports it can allocate to VMs (PLT-452).
	ReserveCPU       int64
	ReserveMemoryMiB int64
	CPUOvercommit    float64
	Labels           string
	// Router overrides the herd-router tunnel address a join handed out.
	Router   string
	JoinOnly bool

	Kernel        string
	Initrd        string
	Cmdline       string
	VCPUs         int
	MemoryMiB     int
	Warm          string
	GuestPort     uint
	CHBinary      string
	Virtiofsd     string
	VirtiofsdArgs string
	// VMCgroup is herd's cgroup under the cgroup v2 root that each VM's processes run in, one cgroup per VM (-vm-cgroup,
	// PLT-214); empty leaves them in the herdlet's own. VMMemoryOverheadMiB is a VM cgroup's room beyond its guest memory.
	VMCgroup            string
	VMMemoryOverheadMiB int
	MacHost             string
	// EgressAllow are the operator's lab allowances (-egress-allow-prefix): the herdlet reports them in its
	// capabilities, a Mac's in-process herd-egress applies them, and herd all-in-one hands them to its herd-egress.
	// On a Linux host that runs herd-egress apart, give herd-egress the same flag.
	EgressAllow list
	// Secrets is a Mac's in-process herd-egress's -secrets (ADR A-21): the directory it resolves
	// ate-secret://dir/<name>/<tail> credentials from. herd all-in-one hands it its own -secrets. On a Linux host,
	// whose herd-egress runs apart, give herd-egress the flag.
	Secrets string
	// RegistryAuth is the node's registry logins for image pulls (-registry-auth, PLT-222): a docker config file
	// (`auths`) read at every pull, so a changed file needs no restart. Empty, or no file there: anonymous pulls.
	RegistryAuth string
	// InsecureRegistries are the registry hosts image pulls speak plain HTTP to besides localhost and 127.0.0.1
	// (-insecure-registry host[:port], PLT-224), exactly as named; every other host is https. -registry-auth's logins
	// apply to them as to any host.
	InsecureRegistries registryHosts

	// EgressBindings, EgressCA, EgressProxyPort, EgressDNSPort and NetPool are a Linux host's VM networks (ADR A-18,
	// linuxNet): herd-egress's bindings file and CA, its transparent and DNS ports, and the pool VM links are leased from.
	EgressBindings  string
	EgressCA        string
	EgressProxyPort uint
	EgressDNSPort   uint
	NetPool         string
	HostFirewall    string

	// VMM and Guests, if set, replace the host's hypervisor (tests run the fake here); Images, if set with them,
	// gives the herdlet machines over that VMM, as the real host has.
	VMM    vmm.VMM
	Guests herdlet.GuestDialer
	Images machine.Images
	// NetHost, if set with VMM, gives the VMs a Linux host's networks (linuxNet) through it in place of ip(8) and
	// nft(8): tests run a fake, so leases, bindings and teardown are the host's own.
	NetHost nft.Host
	// OnActor, if set with VMM, is told which actor a VM is before its boot or restore, as a Mac's in-process herd-egress
	// is (Backend.OnActor): tests give the fake's VMs a network served under the actor's egress policy.
	OnActor func(vm string, a link.ActorRef)
	// MacNet, if set with VMM, gives the VMs a Mac's network (ADR A-21) in place of a Linux host's: the in-process
	// herd-egress a Mac builds (macNet), behind one gvisor-tap-vsock stack per VM. build hands MacNet the function
	// vz attaches each VM's NIC with on a Mac (vz.Config.Network), so a test attaches the guest's end itself (gvvm).
	MacNet func(attach func(vm string) (*os.File, error))
	// NewGateway, if set, builds a Mac's in-process herd-egress from the configuration the flags gave instead of
	// egress.New (tests: a resolver and dialer of their own).
	NewGateway func(egress.Config) (*egress.Gateway, error)
	// Retry is how long the herdlet waits before it dials again after its link or tunnel broke (default 2 s).
	Retry time.Duration
	// Identity is the node's SPIFFE identity, spiffe://<td>/herd/node/<worker> (the Workload API's by default): the
	// herdlet joins, links to herd-api and opens its tunnel to herd-router over TLS as it (ADR 0001 A-60).
	Identity Identity
	src      auth.Source
	closeID  func()

	// granted, if set, is the snapshot store herd-api hands out at join now (nil inside: a directory): a grant in the
	// state directory that names another is replaced by a new join (an all-in-one, where both are known, PLT-382).
	granted **s3.Config
}

// HostFlags registers the flags every herdlet command shares: the host, its hypervisor and its VMs.
func (h *Herdlet) HostFlags(fs *flag.FlagSet) {
	fs.StringVar(&h.Worker, "worker", "", "worker name (a DNS label; default: the one the node's SVID names, spiffe://<td>/herd/node/<worker>)")
	h.Identity.Flags(fs)
	fs.StringVar(&h.Labels, "labels", "", "worker labels, k=v,k=v")
	fs.IntVar(&h.Slots, "slots", 0, "actors this host holds at most (0: no count ceiling; its cpu and memory decide)")
	fs.Int64Var(&h.ReserveCPU, "reserve-cpu", 1000, "millicores of this host kept from VMs")
	fs.Int64Var(&h.ReserveMemoryMiB, "reserve-memory", 2048, "MiB of this host's memory kept from VMs")
	fs.Float64Var(&h.CPUOvercommit, "cpu-overcommit", 1, "vCPUs a core carries when counting what VMs may take")
	fs.StringVar(&h.StateDir, "state", "/var/lib/herdlet", "state directory: join grant, VMs, shares, image cache")
	fs.StringVar(&h.Snapshots, "snapshots", "", "snapshot store directory, herd-api's, for a herd-api without -s3 (default: <state>/snapshots; ignored when the join handed out an S3 store)")
	fs.IntVar(&h.ImageCacheMiB, "image-cache", DefaultImageCacheMiB, "MiB of rootfs images and layer blobs kept in <state>/images, least recently used removed first, never one a VM runs on (0: no bound)")
	fs.IntVar(&h.BundleCacheMiB, "bundle-cache", DefaultBundleCacheMiB, "MiB of snapshot bundles kept in <state>/bundles when snapshots are in S3, least recently used evicted first: a restore of a bundle this node wrote or read before (a template's golden) reads it from there, not from S3 (0: none)")
	fs.StringVar(&h.Router, "router", "", "herd-router tunnel address (default: the one herd-api handed out at join)")
	fs.StringVar(&h.Kernel, "kernel", "/usr/local/share/herd/kernel", "guest kernel from guest/build.sh (bzImage on amd64, Image on arm64)")
	fs.StringVar(&h.Initrd, "initrd", "/usr/local/share/herd/initrd.img", "guest initramfs with herd-guest as /init")
	fs.StringVar(&h.Cmdline, "cmdline", "", "guest kernel command line (backend default if empty)")
	fs.IntVar(&h.VCPUs, "vcpus", 1, "vCPUs of a VM whose template sets none")
	fs.IntVar(&h.MemoryMiB, "memory-mib", 512, "memory of a VM whose template sets none")
	fs.StringVar(&h.Warm, "warm", "", "warm VMs per template, <template-uid>=N,...")
	fs.UintVar(&h.GuestPort, "guest-port", 8080, "herd-guest's vsock port (warm VM health check)")
	fs.StringVar(&h.CHBinary, "cloud-hypervisor", "cloud-hypervisor", "Cloud Hypervisor binary (Linux)")
	fs.StringVar(&h.Virtiofsd, "virtiofsd", DefaultVirtiofsd, "virtiofsd binary, >= 1.11 (Linux; default: the one herd's release archive carries, as the runbooks install it)")
	fs.StringVar(&h.VirtiofsdArgs, "virtiofsd-args", "", "extra virtiofsd arguments, space-separated (Linux)")
	fs.StringVar(&h.VMCgroup, "vm-cgroup", cloudhypervisor.DefaultCgroupParent, "cgroup under the cgroup v2 root (/sys/fs/cgroup) each VM's cloud-hypervisor and virtiofsd run in, as <vm-cgroup>/<vm>, outside the herdlet's own (Nomad task, systemd unit); empty: in the herdlet's cgroup, charged to its memory limit (Linux)")
	fs.IntVar(&h.VMMemoryOverheadMiB, "vm-memory-overhead", cloudhypervisor.DefaultMemoryOverheadMiB, "MiB a VM's cgroup may hold beyond its guest memory (its memory.max is guest + this): the VMM, virtiofsd, page cache (Linux)")
	fs.StringVar(&h.MacHost, "host", "", "this Mac's label for its MEMORY restore family (macOS; default: the hostname)")
	fs.StringVar(&h.EgressBindings, "egress-bindings", DefaultEgressBindings, "herd-egress's bindings file (its -bindings): the herdlet binds each VM's address to its actor there (Linux)")
	fs.StringVar(&h.EgressCA, "egress-ca", "", "herd-egress's CA certificate (its -ca-out) the guests trust (Linux; default <state>/egress-ca.pem)")
	fs.UintVar(&h.EgressProxyPort, "egress-proxy-port", 15001, "herd-egress's transparent TCP port each VM's table redirects to (Linux)")
	fs.UintVar(&h.EgressDNSPort, "egress-dns-port", 15053, "herd-egress's DNS port each VM's table redirects UDP 53 to (Linux)")
	fs.StringVar(&h.NetPool, "net-pool", tapnet.DefaultPool.String(), "IPv4 pool each VM's /30 link is leased from (Linux)")
	fs.StringVar(&h.HostFirewall, "host-firewall", tapnet.FirewallInsert, "when another table's input chain drops by policy (ufw, Tailscale): insert each VM's accepts for exactly its flows to herd-egress there and remove them with the VM, or refuse to start VMs (Linux: insert|refuse)")
	fs.Var(&h.EgressAllow, "egress-allow-prefix", egressAllowUsage)
	fs.StringVar(&h.Secrets, "secrets", "", "directory for ate-secret://dir/<name>/<tail> credentials of the in-process herd-egress (macOS; on Linux, herd-egress -secrets)")
	fs.Var(&h.InsecureRegistries, "insecure-registry", "a registry host[:port] image pulls speak plain HTTP to, exactly as written (repeatable, or comma-separated; empty names none), as the docker daemon's insecure-registries; every other host is https, except localhost and 127.0.0.1")
	fs.StringVar(&h.RegistryAuth, "registry-auth", "", "registry logins image pulls use, a docker config file (auths, per registry host: auth = base64 user:password, or identitytoken), read at every pull; empty, or no file there: anonymous pulls")
}

// RunFlags registers `herdlet run`'s flags: the host flags, and a herdlet credential to dial with instead of a join.
func (h *Herdlet) RunFlags(fs *flag.FlagSet) {
	h.HostFlags(fs)
	fs.StringVar(&h.API, "api", "", "herd-api address: the first start asks it for the node's settings by the node's SVID (default: the address recorded then)")
	fs.StringVar(&h.CredentialFile, "token-file", "", "break-glass only: an HS256 herdlet credential (herd-api token -herdlet); without it, the grant an earlier join left in -state")
}

// JoinFlags registers `herdlet join`'s flags: the host flags plus the join token.
func (h *Herdlet) JoinFlags(fs *flag.FlagSet) {
	h.HostFlags(fs)
	fs.StringVar(&h.Token, "token", "", "break-glass only: a join token from `herd-api join-token` (a node joins by its SVID otherwise)")
	fs.StringVar(&h.TokenFile, "token-file", "", "break-glass only: a file holding the join token")
	fs.BoolVar(&h.JoinOnly, "join-only", false, "join, write the grant to the state directory and exit")
}

func defaultWorker() string {
	n, err := os.Hostname()
	if err != nil {
		return ""
	}
	n = strings.ToLower(strings.SplitN(n, ".", 2)[0])
	return strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(n, "-"), "-")
}

// State is what a join leaves in the state directory (join.json): herd-api's address and the grant.
type State struct {
	API   string     `json:"api"`
	Grant join.Grant `json:"grant"`
}

// StatePath is the join state file in dir.
func StatePath(dir string) string { return filepath.Join(dir, "join.json") }

// EgressTokenPath is where a join writes the egress credential for the host's herd-egress (-token-file).
func EgressTokenPath(dir string) string { return filepath.Join(dir, "egress.token") }

func readState(dir string) (State, error) {
	b, err := os.ReadFile(StatePath(dir))
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", StatePath(dir), err)
	}
	return s, nil
}

func writeState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(StatePath(dir)+".tmp", b, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(EgressTokenPath(dir), []byte(s.Grant.EgressCredential+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(StatePath(dir)+".tmp", StatePath(dir))
}

// identity opens the node's SPIFFE identity once: it must be a node SVID, and the worker is the one it names
// (-worker, if given, must agree).
func (h *Herdlet) identity(ctx context.Context) (auth.Source, error) {
	if h.src != nil {
		return h.src, nil
	}
	src, closer, err := h.Identity.Open(ctx, "herdlet")
	if err != nil {
		return nil, err
	}
	self, err := auth.Self(src)
	if err == nil {
		var id auth.Identity
		if id, err = auth.ParseID(self); err == nil && id.Kind != auth.KindNode {
			err = fmt.Errorf("the herdlet's SVID %s is not a node's (spiffe://%s/herd/node/<worker>)", self, self.TrustDomain().Name())
		} else if err == nil && h.Worker != "" && h.Worker != id.Worker {
			err = fmt.Errorf("-worker %q, but the node's SVID %s is worker %q's", h.Worker, self, id.Worker)
		} else if err == nil {
			h.Worker = id.Worker
		}
	}
	if err != nil {
		closer()
		return nil, fmt.Errorf("herdlet: %w", err)
	}
	h.src, h.closeID = src, closer
	return src, nil
}

// apiID is the SPIFFE ID the herdlet dials herd-api as.
func apiID(src auth.Source) (spiffeid.ID, error) {
	self, err := auth.Self(src)
	if err != nil {
		return spiffeid.ID{}, err
	}
	return auth.HerdID(self.TrustDomain(), "api"), nil
}

// Join returns this host's grant: the one a previous join of the same worker to the same herd-api left in the state
// directory, or a new one, asked for as the node's SVID (or, under the break-glass, with a join token).
func (h *Herdlet) Join(ctx context.Context) (State, error) {
	src, err := h.identity(ctx)
	if err != nil {
		return State{}, err
	}
	if err := join.ValidWorker(h.Worker); err != nil {
		return State{}, fmt.Errorf("-worker: %w", err)
	}
	// A token join's HS256 credential is short-lived (join.DefaultCredentialTTL): a grant whose credential ended is
	// joined again with the token.
	if s, err := readState(h.StateDir); err == nil && s.Grant.Worker == h.Worker && (h.API == "" || s.API == h.API) &&
		(s.Grant.Credential == "" || time.Now().Unix() < s.Grant.ExpiresAt) {
		if h.granted == nil || sameStore(s.Grant.Snapshots, *h.granted) {
			return s, nil
		}
		log.Printf("herdlet: the join grant in %s names snapshot store %s, herd-api hands out %s: joining again",
			StatePath(h.StateDir), SnapshotStoreName(s.Grant.Snapshots), SnapshotStoreName(*h.granted))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	if h.API == "" {
		return State{}, errors.New("no herd-api to ask for this node's settings yet: give -api <herd-api>")
	}
	tok := h.Token
	if tok == "" && h.TokenFile != "" {
		b, err := os.ReadFile(h.TokenFile)
		if err != nil {
			return State{}, err
		}
		tok = strings.TrimSpace(string(b))
	}
	api, err := apiID(src)
	if err != nil {
		return State{}, err
	}
	cc, err := dial(h.API, src, api)
	if err != nil {
		return State{}, err
	}
	defer cc.Close()
	g, err := join.Call(ctx, cc, tok, h.Worker)
	if err != nil {
		return State{}, fmt.Errorf("join %s as %s: %w", h.API, h.Worker, err)
	}
	s := State{API: h.API, Grant: g}
	if err := writeState(h.StateDir, s); err != nil {
		return State{}, err
	}
	log.Printf("herdlet: joined %s as %s", h.API, h.Worker)
	return s, nil
}

// credentials returns what the herdlet dials with: a herdlet credential file given on the command line (no join), or
// the join grant.
func (h *Herdlet) credentials(ctx context.Context) (State, error) {
	if _, err := h.identity(ctx); err != nil {
		return State{}, err
	}
	if h.CredentialFile == "" {
		return h.Join(ctx)
	}
	if h.API == "" || h.Worker == "" {
		return State{}, errors.New("-token-file needs -api and -worker")
	}
	b, err := os.ReadFile(h.CredentialFile)
	if err != nil {
		return State{}, err
	}
	return State{API: h.API, Grant: join.Grant{Worker: h.Worker, Credential: strings.TrimSpace(string(b))}}, nil
}

func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("label %q: want k=v", kv)
		}
		out[k] = v
	}
	return out, nil
}

func parseWarm(s string) ([]link.Template, error) {
	var out []link.Template
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		uid, n, ok := strings.Cut(kv, "=")
		w, err := strconv.Atoi(n)
		if !ok || uid == "" || err != nil || w < 0 {
			return nil, fmt.Errorf("-warm %q: want <template-uid>=N", kv)
		}
		// Declared by count: the spec arrives with the template's first boot (machine.Pool).
		out = append(out, link.Template{UID: uid, Warm: w})
	}
	return out, nil
}

// Backend is what a hypervisor backend gives the herdlet.
type Backend struct {
	VMM    vmm.VMM
	Guests machine.GuestDialer
	// Version is the hypervisor's, reported with the capabilities.
	Version string
	// OnActor and Serve are a Mac's in-process herd-egress (ADR A-21): OnActor tells it which actor a VM is, and
	// Serve keeps the actors' egress policies current, read over policies (the join's egress credential), until ctx
	// ends. Nil on Linux, whose herd-egress is its own process.
	OnActor func(vm string, a link.ActorRef)
	Serve   func(ctx context.Context, policies grpc.ClientConnInterface)
	// Network gives each VM its link (machine.Network): TAPs on Linux, the stack's fixed link on a Mac.
	Network machine.Network
}

// build makes the host's backend and the machines that turn templates into VMs on it: the image cache (OCI
// registry, erofs or ext4 by mkfs.Detect), one share per VM and the base spec. why says which rootfs builder was
// chosen. A herdlet given a VMM uses it, with machines only when it was also given Images.
func (h *Herdlet) build(ctx context.Context) (Backend, *machine.Machines, string, error) {
	base := vmm.Spec{Kernel: h.Kernel, Initrd: h.Initrd, Cmdline: h.Cmdline, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB}
	shares := filepath.Join(h.StateDir, "shares")
	if h.VMM != nil {
		host := Backend{VMM: h.VMM, Guests: h.Guests, OnActor: h.OnActor}
		if h.NetHost != nil {
			n, err := h.linuxNet(h.NetHost)
			if err != nil {
				return Backend{}, nil, "", err
			}
			host.Network, host.OnActor = n, assign(n, h.OnActor)
		}
		if h.MacNet != nil {
			label := h.MacHost
			if label == "" {
				label = "mac"
			}
			api := h.API
			if s, err := readState(h.StateDir); err == nil && api == "" {
				api = s.API
			}
			mac, nw, err := h.macNet(api, label)
			if err != nil {
				return Backend{}, nil, "", err
			}
			onActor := h.OnActor
			host.Network, host.Serve = mac.Network, mac.Serve
			host.OnActor = func(vm string, a link.ActorRef) {
				mac.OnActor(vm, a)
				if onActor != nil {
					onActor(vm, a)
				}
			}
			h.MacNet(nw.Attach)
		}
		if h.Images == nil {
			return host, nil, "given", nil
		}
		m, err := machine.New(machine.Config{VMM: h.VMM, Guests: h.Guests, Flush: h.flusher(h.Guests), Clock: h.clocker(h.Guests), ShareDir: shares, Images: h.Images, Base: base, Network: host.Network})
		return host, m, "given", err
	}
	host, err := h.newHost(ctx)
	if err != nil {
		return Backend{}, nil, "", err
	}
	caps, err := host.VMM.Capabilities(ctx)
	if err != nil {
		return Backend{}, nil, "", fmt.Errorf("capabilities: %w", err)
	}
	builder, why, err := mkfs.Detect(ctx, mkfs.Exec{})
	if err != nil {
		return Backend{}, nil, why, err
	}
	images := &image.Cache{Dir: filepath.Join(h.StateDir, "images"), Source: h.imageSource(), Builder: builder,
		Platform: image.Platform{OS: "linux", Architecture: caps.Arch}, MaxBytes: int64(h.ImageCacheMiB) << 20}
	m, err := machine.New(machine.Config{
		VMM: host.VMM, Guests: host.Guests, Flush: h.flusher(host.Guests), Clock: h.clocker(host.Guests), ShareDir: shares,
		Network: host.Network, Images: images, Base: base,
	})
	if m != nil {
		images.InUse = m.ImagePaths
	}
	return host, m, why, err
}

// Describe prints what this host declares and why, without dialing: the rootfs builder, the platform and the
// suspend fidelity and restore families (the second half of `herdlet doctor`).
func (h *Herdlet) Describe(ctx context.Context, out io.Writer) error {
	host, _, why, err := h.build(ctx)
	fmt.Fprintf(out, "rootfs images: %s\n", why)
	if err != nil {
		return err
	}
	caps, err := host.VMM.Capabilities(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "platform %s, arch %s (runtime %s), hypervisor %s\nsuspend max fidelity %v, restore family %q, portable family %q\n",
		caps.Platform, caps.Arch, runtime.GOARCH, host.Version, caps.SuspendMaxFidelity, caps.RestoreFamily, vmm.PortableRestoreFamily)
	return nil
}

// Run joins (or reuses its grant, or dials with -token-file) and serves work from herd-api, and the router's tunnel,
// until ctx ends. A link or tunnel that breaks is dialed again.
func (h *Herdlet) Run(ctx context.Context) error {
	if err := h.preflight(); err != nil {
		return err
	}
	if err := labAllow("herdlet", h.EgressAllow); err != nil {
		return err
	}
	st, err := h.credentials(ctx)
	if err != nil {
		return err
	}
	defer h.closeID()
	ctx, changed := pinIdentity(ctx, h.src, "herdlet")
	if h.JoinOnly {
		return nil
	}
	labels, err := parseLabels(h.Labels)
	if err != nil {
		return err
	}
	warm, err := parseWarm(h.Warm)
	if err != nil {
		return err
	}
	milliCPU, memoryMiB, err := h.allocatable(hostResources())
	if err != nil {
		return err
	}
	log.Printf("herdlet: allocatable %dm cpu, %d MiB memory, slot ceiling %d", milliCPU, memoryMiB, h.Slots)
	snaps, err := h.snapshots(st.Grant)
	if err != nil {
		return err
	}
	bundles, err := h.bundles(st.Grant)
	if err != nil {
		return err
	}
	host, m, why, err := h.build(ctx)
	if err != nil {
		return err
	}
	log.Printf("herdlet: rootfs images: %s", why)
	guest, err := GuestIdentity(h.Kernel, h.Initrd)
	if err != nil {
		// A golden this herdlet builds then records no guest, and an upgrade does not rebuild it (DEFAUL-1606).
		log.Printf("herdlet: guest build not known: %v", err)
	} else {
		log.Printf("herdlet: guest build %s", guest)
	}
	agent := &herdlet.Agent{Worker: st.Grant.Worker, Class: apiserver.SandboxClass, Labels: labels, Slots: h.Slots, MilliCPU: milliCPU, MemoryMiB: memoryMiB, OverheadMiB: h.VMMemoryOverheadMiB, VMM: host.VMM,
		Snapshots: snaps, Bundles: bundles, HypervisorVersion: host.Version, Machines: m, EgressAllowPrefixes: h.EgressAllow, Guest: guest,
		Boot: vmm.Spec{Kernel: h.Kernel, Initrd: h.Initrd, Cmdline: h.Cmdline, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB}}
	// Capabilities are re-read every 30 s and on each wake, and a change is pushed to herd-api (a Mac whose console
	// locked is VOLUMES at once).
	agent.OnActor, agent.Wake = host.OnActor, herdlet.WakeDetector{}.Run(ctx)
	// The VM snapshots this host keeps are bounded (PLT-374): a start removes those no snapshot object holds, and what
	// a snapshot cut short left.
	agent.SnapshotRefs = herdlet.SnapshotRefsPath(h.StateDir)
	agent.SnapshotStore = SnapshotStoreName(st.Grant.Snapshots)
	if err := agent.SweepSnapshots(ctx, true); err != nil {
		log.Printf("herdlet: VM snapshots not swept at start: %v", err)
	}
	var guests herdlet.GuestDialer = h.Guests
	if m != nil {
		guests = m
		if len(warm) > 0 {
			agent.Pool = &machine.Pool{Machines: m, Health: machine.GuestHealth{VMM: host.VMM, Guests: host.Guests, Port: uint32(h.GuestPort)}}
			agent.Pool.Set(warm)
			defer agent.Pool.Drain(context.WithoutCancel(ctx))
		}
	} else if len(warm) > 0 {
		return errors.New("-warm needs the host's machines (a herdlet given a bare VMM has none)")
	}
	retry := h.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	tunnelAddr := h.Router
	if tunnelAddr == "" {
		tunnelAddr = st.Grant.Tunnel
	}
	src, err := h.identity(ctx)
	if err != nil {
		return err
	}
	api, err := apiID(src)
	if err != nil {
		return err
	}
	// The herdlet is its node's SVID; an HS256 credential (a break-glass join or -token-file) rides on top of it.
	var creds []grpc.DialOption
	if st.Grant.Credential != "" {
		creds = append(creds, grpc.WithPerRPCCredentials(auth.Bearer(st.Grant.Credential)))
	}
	// herd-api and herd-router permit keepalive pings every 10 s; they keep a link behind NAT open.
	keep := grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 15 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	if tunnelAddr != "" && guests != nil {
		agent.Tunnel = &herdlet.Tunnel{Worker: st.Grant.Worker, Guests: guests}
		dialTunnel := func() (*grpc.ClientConn, error) {
			return dial(tunnelAddr, src, auth.HerdID(api.TrustDomain(), "router"), append(creds, keep)...)
		}
		tc, err := dialTunnel()
		if err != nil {
			return err
		}
		go func() {
			defer close(done)
			generations(ctx, "tunnel to "+tunnelAddr, retry, dialTunnel, rotations(ctx, src), st.Grant.Credential == "", true, tc,
				func(cc *grpc.ClientConn) error { return agent.Tunnel.Run(ctx, cc) }, func(error) bool { return false })
		}()
	} else {
		close(done)
		log.Printf("herdlet: no herd-router tunnel (none advertised, or no guest dialer)")
	}
	dialAPI := func() (*grpc.ClientConn, error) { return dial(st.API, src, api, append(creds, keep)...) }
	cc, err := dialAPI()
	if err != nil {
		return err
	}
	if host.Serve != nil {
		// The in-process herd-egress reads the egress policies as the node (its SVID's built-in grant), or with the
		// break-glass egress credential a token join granted.
		eopts := []grpc.DialOption{keep}
		if st.Grant.EgressCredential != "" {
			eopts = append(eopts, grpc.WithPerRPCCredentials(auth.Bearer(st.Grant.EgressCredential)))
		}
		ec, err := redialing(st.API, src, api, eopts...)
		if err != nil {
			return err
		}
		defer ec.Close()
		go host.Serve(ctx, ec)
	}
	log.Printf("herdlet %s: linking to herd-api at %s", st.Grant.Worker, st.API)
	var refused error
	// The link runs make-before-break across the node SVID's rotations (re-review 3): a rotation opens a new link with
	// the current SVID, herd-api moves the worker's calls to it, and the older link ends once its calls are answered.
	generations(ctx, "link to "+st.API, retry, dialAPI, rotations(ctx, src), st.Grant.Credential == "", false, cc,
		func(cc *grpc.ClientConn) error { return agent.Run(ctx, cc) },
		func(err error) bool {
			if status.Code(err) == codes.FailedPrecondition {
				// herd-api refuses this host as it is (its snapshot store is not herd-api's): a retry cannot change that.
				refused = err
				cancel()
				return true
			}
			if st.Grant.Credential != "" && status.Code(err) == codes.Unauthenticated {
				// The break-glass credential ended (or the allowance did): the next start joins again with the token.
				refused = err
				cancel()
				return true
			}
			return false
		})
	<-done
	if cerr := changed(); cerr != nil {
		return cerr
	}
	if refused != nil {
		return fmt.Errorf("%w (this host's join grant: %s; remove it and join again with a new join token)", refused, StatePath(h.StateDir))
	}
	return nil
}

// RotationCheck is how often a herdlet looks for a rotated node SVID to link again with.
var RotationCheck = 5 * time.Second

// rotations signals each time src's X.509-SVID is another certificate than the last one seen.
func rotations(ctx context.Context, src auth.Source) <-chan struct{} {
	out := make(chan struct{}, 1)
	serial := func() string {
		sv, err := src.GetX509SVID()
		if err != nil || len(sv.Certificates) == 0 {
			return ""
		}
		return sv.Certificates[0].SerialNumber.String()
	}
	last := serial()
	go func() {
		t := time.NewTicker(RotationCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if now := serial(); now != "" && now != last {
				last = now
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}

// generations runs run over cc until ctx ends, make-before-break: each rotation dials a new connection (presenting
// the current SVID) and starts a new generation on it while the older one goes on until its server lets it go, and
// is not retried then. keepOld keeps a superseded generation's connection open until ctx ends: a tunnel's data streams
// ride on it. A generation whose run ends with svid set and Unavailable (its connection draining) or
// Unauthenticated dials again before it retries. stop, true, ends every generation.
func generations(ctx context.Context, what string, retry time.Duration, dialFn func() (*grpc.ClientConn, error), rotate <-chan struct{},
	svid, keepOld bool, cc *grpc.ClientConn, run func(*grpc.ClientConn) error, stop func(error) bool) {
	var gen atomic.Int64
	var wg sync.WaitGroup
	kept := make(chan *grpc.ClientConn, 1024)
	defer func() {
		close(kept)
		for k := range kept {
			k.Close()
		}
	}()
	start := func(cc *grpc.ClientConn) {
		g := gen.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if cc != nil {
					cc.Close()
				}
			}()
			for ctx.Err() == nil {
				err := run(cc)
				if gen.Load() != g {
					if keepOld {
						// The data streams this connection carries (a tunnel's) stay up until they end; the server
						// closes the connection then, and it is let go with ctx.
						kept <- cc
						cc = nil
					}
					return // a newer generation took over
				}
				if err != nil && ctx.Err() == nil {
					log.Printf("herdlet: %s: %v", what, err)
				}
				if stop(err) {
					return
				}
				if c := status.Code(err); svid && (c == codes.Unavailable || c == codes.Unauthenticated) {
					if n, derr := dialFn(); derr == nil {
						cc.Close()
						cc = n
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(retry):
				}
			}
		}()
	}
	start(cc)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-rotate:
			if n, err := dialFn(); err == nil {
				log.Printf("herdlet: %s: the node SVID rotated; opening a new one before the old one goes", what)
				start(n)
			}
		}
	}
}

// forever runs f until ctx ends, waiting retry after each return.
func forever(ctx context.Context, retry time.Duration, what string, f func() error) {
	for ctx.Err() == nil {
		if err := f(); err != nil && ctx.Err() == nil {
			log.Printf("herdlet: %s: %v", what, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
	}
}

// thisHost is the host preflight checks; tests replace it.
var thisHost = ThisHost

// preflight refuses a host that cannot run microVMs before it joins (R-D7); doctor says why. A herdlet given its
// VMM (tests) and a join-only run skip it.
func (h *Herdlet) preflight() error {
	if h.VMM != nil || h.JoinOnly {
		return nil
	}
	var failed []string
	for _, c := range Doctor(h, thisHost()) {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("this host cannot run herd actors (see herdlet doctor):\n  %s", strings.Join(failed, "\n  "))
	}
	return nil
}

// flusher is how machines have a guest flush before a snapshot: herd-guest's Sync on the guest port. Without a
// guest dialer there is no guest to ask.
func (h *Herdlet) flusher(g machine.GuestDialer) machine.Flusher {
	if g == nil {
		return nil
	}
	port := uint32(h.GuestPort)
	if port == 0 {
		port = 8080
	}
	return machine.GuestFlush{Guests: g, Port: port}
}

// clocker is how machines hand a guest the host's wall clock when it runs on from a snapshot's memory (DEFAUL-1595):
// herd-guest's SetClock on the guest port. Without a guest dialer there is no guest to ask.
func (h *Herdlet) clocker(g machine.GuestDialer) machine.Clocker {
	if g == nil {
		return nil
	}
	port := uint32(h.GuestPort)
	if port == 0 {
		port = 8080
	}
	return machine.GuestClock{Guests: g, Port: port}
}

// snapshots opens the herdlet's snapshot store: the S3 store herd-api handed out at join (no shared filesystem), else
// the -snapshots directory, else <state>/snapshots. herd-api verifies every snapshot in its own store, so a grant's
// store wins over -snapshots, which is ignored with a warning.
func (h *Herdlet) snapshots(g join.Grant) (snapstore.Store, error) {
	if g.Snapshots != nil {
		s, err := s3.New(*g.Snapshots, nil)
		if err != nil {
			return nil, fmt.Errorf("the join's snapshot store: %w", err)
		}
		if h.Snapshots != "" {
			log.Printf("herdlet: warning: -snapshots %s ignored: the join's grant names herd-api's S3 store, where herd-api verifies every snapshot (drop -snapshots from this host's unit)", h.Snapshots)
		}
		log.Printf("herdlet: snapshots in s3://%s/%s at %s (from the join)", g.Snapshots.Bucket, g.Snapshots.Prefix, g.Snapshots.Endpoint)
		return s, nil
	}
	dir := h.Snapshots
	if dir == "" {
		dir = filepath.Join(h.StateDir, "snapshots")
	}
	return disk.New(dir)
}

// SnapshotStoreName names a join grant's snapshot store with no credential in it: its S3 bucket, or a directory.
func SnapshotStoreName(g *s3.Config) string {
	if g == nil {
		return "a directory (herd-api without -s3)"
	}
	return fmt.Sprintf("s3://%s at %s", path.Join(g.Bucket, g.Prefix), g.Endpoint)
}

// sameStore: two grants' snapshot stores are the same store, reached with the same credentials.
func sameStore(a, b *s3.Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DefaultImageCacheMiB is -image-cache's default.
const DefaultImageCacheMiB = 20 << 10

// DefaultBundleCacheMiB is -bundle-cache's default.
const DefaultBundleCacheMiB = 20 << 10

// bundles is the node's bundle cache: none when snapshots are on a local or shared disk, which a restore reads as
// fast as a copy, or when -bundle-cache is 0.
func (h *Herdlet) bundles(g join.Grant) (*bundlecache.Cache, error) {
	if g.Snapshots == nil || h.BundleCacheMiB <= 0 {
		return nil, nil
	}
	c, err := bundlecache.New(filepath.Join(h.StateDir, "bundles"), int64(h.BundleCacheMiB)<<20)
	if err != nil {
		return nil, err
	}
	log.Printf("herdlet: snapshot bundles kept in %s, up to %d MiB", filepath.Join(h.StateDir, "bundles"), h.BundleCacheMiB)
	return c, nil
}

// imageSource is the registry the herdlet pulls template images from, logged in with -registry-auth's credentials
// and speaking plain HTTP to the -insecure-registry hosts.
func (h *Herdlet) imageSource() *registry.Client {
	c := &registry.Client{}
	if len(h.InsecureRegistries) > 0 {
		c.PlainHTTP = registry.PlainHosts(h.InsecureRegistries)
	}
	if h.RegistryAuth != "" {
		c.Credentials = registry.DockerConfig{Path: h.RegistryAuth}
	}
	return c
}

// registryHosts is -insecure-registry's value: registry host[:port] names, each checked to be one (no scheme, no path).
type registryHosts []string

func (r *registryHosts) String() string { return strings.Join(*r, ",") }

func (r *registryHosts) Set(v string) error {
	for _, h := range strings.Split(v, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if err := registry.CheckHost(h); err != nil {
			return err
		}
		*r = append(*r, h)
	}
	return nil
}
