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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

	// granted, if set, is the snapshot store herd-api hands out at join now (nil inside: a directory): a grant in the
	// state directory that names another is replaced by a new join (an all-in-one, where both are known, PLT-382).
	granted **s3.Config
}

// HostFlags registers the flags every herdlet command shares: the host, its hypervisor and its VMs.
func (h *Herdlet) HostFlags(fs *flag.FlagSet) {
	fs.StringVar(&h.Worker, "worker", defaultWorker(), "worker name (a DNS label; default: this host's name)")
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
	fs.StringVar(&h.API, "api", "", "herd-api address (with -token-file; default: the one the join recorded)")
	fs.StringVar(&h.CredentialFile, "token-file", "", "herdlet credential (herd-api token -herdlet); without it, the grant an earlier join left in -state")
}

// JoinFlags registers `herdlet join`'s flags: the host flags plus the join token.
func (h *Herdlet) JoinFlags(fs *flag.FlagSet) {
	h.HostFlags(fs)
	fs.StringVar(&h.Token, "token", "", "join token from `herd-api join-token`")
	fs.StringVar(&h.TokenFile, "token-file", "", "file holding the join token")
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

// Join returns this host's grant: the one a previous join of the same worker to the same herd-api left in the state
// directory, or a new one for the join token.
func (h *Herdlet) Join(ctx context.Context) (State, error) {
	if err := join.ValidWorker(h.Worker); err != nil {
		return State{}, fmt.Errorf("-worker: %w", err)
	}
	if s, err := readState(h.StateDir); err == nil && s.Grant.Worker == h.Worker && (h.API == "" || s.API == h.API) {
		if h.granted == nil || sameStore(s.Grant.Snapshots, *h.granted) {
			return s, nil
		}
		log.Printf("herdlet: the join grant in %s names snapshot store %s, herd-api hands out %s: joining again",
			StatePath(h.StateDir), SnapshotStoreName(s.Grant.Snapshots), SnapshotStoreName(*h.granted))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	if h.API == "" {
		return State{}, errors.New("not joined yet: run `herdlet join <herd-api> --token <t>`")
	}
	tok := h.Token
	if tok == "" && h.TokenFile != "" {
		b, err := os.ReadFile(h.TokenFile)
		if err != nil {
			return State{}, err
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		return State{}, errors.New("a join needs --token or --token-file")
	}
	cc, err := grpc.NewClient("passthrough:///"+h.API, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	cred := grpc.WithPerRPCCredentials(auth.Bearer(st.Grant.Credential))
	// herd-api and herd-router permit keepalive pings every 10 s; they keep a link behind NAT open.
	keep := grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 15 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	if tunnelAddr != "" && guests != nil {
		agent.Tunnel = &herdlet.Tunnel{Worker: st.Grant.Worker, Guests: guests}
		tc, err := grpc.NewClient("passthrough:///"+tunnelAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), cred, keep)
		if err != nil {
			return err
		}
		defer tc.Close()
		go func() {
			defer close(done)
			forever(ctx, retry, "tunnel to "+tunnelAddr, func() error { return agent.Tunnel.Run(ctx, tc) })
		}()
	} else {
		close(done)
		log.Printf("herdlet: no herd-router tunnel (none advertised, or no guest dialer)")
	}
	cc, err := grpc.NewClient("passthrough:///"+st.API, grpc.WithTransportCredentials(insecure.NewCredentials()), cred, keep)
	if err != nil {
		return err
	}
	defer cc.Close()
	if host.Serve != nil {
		if st.Grant.EgressCredential == "" {
			log.Printf("herdlet: no egress credential (a -token-file run): every actor on this host stays deny-all")
		}
		ec, err := grpc.NewClient("passthrough:///"+st.API, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(auth.Bearer(st.Grant.EgressCredential)), keep)
		if err != nil {
			return err
		}
		defer ec.Close()
		go host.Serve(ctx, ec)
	}
	log.Printf("herdlet %s: linking to herd-api at %s", st.Grant.Worker, st.API)
	var refused error
	forever(ctx, retry, "link to "+st.API, func() error {
		err := agent.Run(ctx, cc)
		if status.Code(err) == codes.FailedPrecondition {
			// herd-api refuses this host as it is (its snapshot store is not herd-api's): a retry cannot change that.
			refused = err
			cancel()
		}
		return err
	})
	<-done
	if refused != nil {
		return fmt.Errorf("%w (this host's join grant: %s; remove it and join again with a new join token)", refused, StatePath(h.StateDir))
	}
	return nil
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
