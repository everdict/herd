// Command herdlet is the node agent (spec R-D3, ADR A-13, A-19): it dials herd-api (and herd-router, if given),
// registers what its host declares, and turns actor templates into microVMs on the host's hypervisor — Cloud
// Hypervisor on Linux, Virtualization.framework on a Mac — with rootfs images built from the templates' OCI images,
// DurableDir volumes in each VM's virtio-fs share, and warm VMs per template.
//
//	herdlet -api 10.0.0.1:7443 -token-file /etc/herd/node.jwt -worker node-1 -labels host=ubuntu \
//	        -state /var/lib/herdlet -snapshots /srv/herd/snapshots -kernel /usr/share/herd/vmlinux \
//	        -initrd /usr/share/herd/initramfs -warm <template-uid>=2
//	herdlet doctor -state /var/lib/herdlet ...       says what this host declares and why, without dialing
//
// The snapshot store (-snapshots) must be the directory herd-api verifies snapshots in: on several hosts, a shared
// filesystem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/mkfs"
	"github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// options is everything the flags set.
type options struct {
	api, router, tokenFile, worker, labels string
	slots                                  int
	state, snapshots                       string
	kernel, initrd, cmdline                string
	vcpus, memMiB                          int
	warm                                   string
	guestPort                              uint
	// Backend-specific.
	chBinary, virtiofsd, virtiofsdArgs, vzHost string
}

func flags(name string, o *options) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&o.api, "api", "127.0.0.1:7443", "herd-api address")
	fs.StringVar(&o.router, "router", "", "herd-router tunnel address (none: no tunnel)")
	fs.StringVar(&o.tokenFile, "token-file", "", "herdlet credential (herd-api token -herdlet)")
	fs.StringVar(&o.worker, "worker", "", "worker name (the credential's subject)")
	fs.StringVar(&o.labels, "labels", "", "worker labels, k=v,k=v")
	fs.IntVar(&o.slots, "slots", 4, "actors this host holds")
	fs.StringVar(&o.state, "state", "/var/lib/herdlet", "state directory: VMs, shares, image cache")
	fs.StringVar(&o.snapshots, "snapshots", "", "snapshot store directory (herd-api's)")
	fs.StringVar(&o.kernel, "kernel", "", "guest kernel (bzImage on amd64, Image on arm64)")
	fs.StringVar(&o.initrd, "initrd", "", "guest initramfs with herd-guest as /init")
	fs.StringVar(&o.cmdline, "cmdline", "", "guest kernel command line (backend default if empty)")
	fs.IntVar(&o.vcpus, "vcpus", 1, "vCPUs of a VM whose template sets none")
	fs.IntVar(&o.memMiB, "memory-mib", 512, "memory of a VM whose template sets none")
	fs.StringVar(&o.warm, "warm", "", "warm VMs per template, <template-uid>=N,...")
	fs.UintVar(&o.guestPort, "guest-port", 8080, "herd-guest's vsock port (warm VM health check)")
	fs.StringVar(&o.chBinary, "cloud-hypervisor", "cloud-hypervisor", "Cloud Hypervisor binary (Linux)")
	fs.StringVar(&o.virtiofsd, "virtiofsd", "/usr/libexec/virtiofsd", "virtiofsd binary (Linux)")
	fs.StringVar(&o.virtiofsdArgs, "virtiofsd-args", "", "extra virtiofsd arguments, space-separated (Linux)")
	fs.StringVar(&o.vzHost, "host", "", "this Mac's label for its MEMORY restore family (macOS; default: the hostname)")
	return fs
}

func main() {
	args := os.Args[1:]
	doctor := len(args) > 0 && args[0] == "doctor"
	if doctor {
		args = args[1:]
	}
	var o options
	fs := flags("herdlet", &o)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if doctor {
		err = runDoctor(ctx, o)
	} else {
		err = run(ctx, o)
	}
	if err != nil {
		log.Fatalf("herdlet: %v", err)
	}
}

// Host is what a backend gives the herdlet.
type Host struct {
	VMM    vmm.VMM
	Guests machine.GuestDialer
	// Version is the hypervisor's, reported with the capabilities.
	Version string
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

func build(ctx context.Context, o options) (Host, *machine.Machines, string, error) {
	h, err := newHost(ctx, o)
	if err != nil {
		return Host{}, nil, "", err
	}
	caps, err := h.VMM.Capabilities(ctx)
	if err != nil {
		return Host{}, nil, "", fmt.Errorf("capabilities: %w", err)
	}
	builder, why, err := mkfs.Detect(ctx, mkfs.Exec{})
	if err != nil {
		return Host{}, nil, why, err
	}
	m, err := machine.New(machine.Config{
		VMM: h.VMM, Guests: h.Guests, ShareDir: filepath.Join(o.state, "shares"),
		Images: &image.Cache{Dir: filepath.Join(o.state, "images"), Source: &registry.Client{}, Builder: builder,
			Platform: image.Platform{OS: "linux", Architecture: caps.Arch}},
		Base: vmm.Spec{Kernel: o.kernel, Initrd: o.initrd, Cmdline: o.cmdline, VCPUs: o.vcpus, MemoryMiB: o.memMiB},
	})
	return h, m, why, err
}

func runDoctor(ctx context.Context, o options) error {
	h, _, why, err := build(ctx, o)
	fmt.Printf("rootfs images: %s\n", why)
	if err != nil {
		return err
	}
	caps, err := h.VMM.Capabilities(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("platform %s, arch %s (runtime %s), hypervisor %s\nsuspend max fidelity %v, restore family %q, portable family %q\n",
		caps.Platform, caps.Arch, runtime.GOARCH, h.Version, caps.SuspendMaxFidelity, caps.RestoreFamily, vmm.PortableRestoreFamily)
	return nil
}

func run(ctx context.Context, o options) error {
	if o.worker == "" || o.tokenFile == "" || o.snapshots == "" || o.kernel == "" {
		return errors.New("-worker, -token-file, -snapshots and -kernel are required")
	}
	labels, err := parseLabels(o.labels)
	if err != nil {
		return err
	}
	warm, err := parseWarm(o.warm)
	if err != nil {
		return err
	}
	tok, err := os.ReadFile(o.tokenFile)
	if err != nil {
		return err
	}
	snaps, err := disk.New(o.snapshots)
	if err != nil {
		return err
	}
	h, m, why, err := build(ctx, o)
	if err != nil {
		return err
	}
	log.Printf("herdlet: rootfs images: %s", why)
	a := &herdlet.Agent{Worker: o.worker, Class: apiserver.SandboxClass, Labels: labels, Slots: o.slots, VMM: h.VMM,
		Snapshots: snaps, HypervisorVersion: h.Version, Machines: m}
	if len(warm) > 0 {
		a.Pool = &machine.Pool{Machines: m, Health: machine.GuestHealth{VMM: h.VMM, Guests: h.Guests, Port: uint32(o.guestPort)}}
		a.Pool.Set(warm)
		defer a.Pool.Drain(context.WithoutCancel(ctx))
	}
	creds := grpc.WithPerRPCCredentials(auth.Bearer(strings.TrimSpace(string(tok))))
	keep := grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 15 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true})
	errc := make(chan error, 2)
	if o.router != "" {
		a.Tunnel = &herdlet.Tunnel{Worker: o.worker, Guests: m}
		rc, err := grpc.NewClient("passthrough:///"+o.router, grpc.WithTransportCredentials(insecure.NewCredentials()), creds, keep)
		if err != nil {
			return err
		}
		defer rc.Close()
		go func() { errc <- redial(ctx, "router", func() error { return a.Tunnel.Run(ctx, rc) }) }()
	}
	cc, err := grpc.NewClient("passthrough:///"+o.api, grpc.WithTransportCredentials(insecure.NewCredentials()), creds, keep)
	if err != nil {
		return err
	}
	defer cc.Close()
	go func() { errc <- redial(ctx, "herd-api", func() error { return a.Run(ctx, cc) }) }()
	log.Printf("herdlet %s: dialing herd-api at %s", o.worker, o.api)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// redial runs f again after it returns, until ctx ends: a broken link is dialed again.
func redial(ctx context.Context, what string, f func() error) error {
	for {
		err := f()
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("herdlet: %s link: %v; dialing again", what, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}
