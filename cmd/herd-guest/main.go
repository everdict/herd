// Command herd-guest is the microVM's init and agent (spec herd.md R5, ADR 0001 A-7: it is /init of the guest
// initramfs). As PID 1 it mounts the guest's pseudo filesystems and the virtio-fs share, creates the DurableDir
// volumes, starts the template's containers with crun, and serves agent-substrate/env's ProcessService and
// FileSystemService (plus herd's FileInfoService) over vsock. The same binary is bound into every container, where
// it runs as the container's init (cinit), as the detached-exec shim (shim) and as the signal helper (signal).
//
//	herd-guest [serve] [-listen vsock://:8080] [-share-tag herd]     PID 1 of the microVM
//	herd-guest serve -local <dir> [-listen unix:///path]              no VM: one local "container" rooted at dir
//	herd-guest cinit -- <argv...>                                      a container's init
//	herd-guest shim <record dir>                                       runs one detached exec record
//	herd-guest signal <pgid> <signal number>                           kill(-pgid, sig) inside a container
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/execrec"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
	"github.com/everdict/herd/guest/transport"
	"github.com/everdict/herd/internal/grpcwire"
)

func main() {
	args := os.Args[1:]
	mode := "serve"
	if len(args) > 0 && (args[0] == "serve" || args[0] == "cinit" || args[0] == "shim" || args[0] == "signal") {
		mode, args = args[0], args[1:]
	}
	var err error
	switch mode {
	case "shim":
		if len(args) != 1 {
			err = fmt.Errorf("usage: herd-guest shim <record dir>")
			break
		}
		err = execrec.Shim(args[0])
	case "signal":
		err = signalGroup(args)
	case "cinit":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		os.Exit(containerInit(args))
	default:
		err = serve(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herd-guest:", err)
		os.Exit(1)
	}
}

func signalGroup(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: herd-guest signal <pgid> <signal number>")
	}
	pgid, err1 := strconv.Atoi(args[0])
	sig, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil || pgid <= 1 || sig < 0 || sig > 64 {
		return fmt.Errorf("signal: bad arguments %q", args)
	}
	return syscall.Kill(-pgid, syscall.Signal(sig))
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "vsock://:8080", "address to serve on: vsock://:<port> or unix://<path>")
	shareTag := fs.String("share-tag", "herd", "virtio-fs tag of the volume share")
	local := fs.String("local", "", "serve one local target rooted at this directory instead of booting containers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var targets []agent.Target
	if *local != "" {
		targets = append(targets, &agent.Local{ContainerName: "local", Root: *local, Dir: "/"})
	} else {
		if os.Getpid() != 1 {
			return fmt.Errorf("serve without -local must run as PID 1 of the microVM")
		}
		startReaper()
		m := initd.KernelMounter{}
		if err := initd.Pseudo(m); err != nil {
			return err
		}
		rt := &oci.Crun{Runner: reapedRunner}
		_, booted, err := initd.Boot(ctx, m, rt, initd.DefaultLayout, *shareTag)
		if err != nil {
			return err
		}
		for _, b := range booted {
			targets = append(targets, &oci.Target{ContainerName: b.Name, RootfsDir: b.Rootfs, Dir: b.Workdir, Runtime: rt})
		}
	}
	g, err := agent.New(targets...)
	if err != nil {
		return err
	}
	defer g.Close()
	l, err := transport.Listen(*listen)
	if err != nil {
		return err
	}
	srv := grpcwire.NewServer()
	g.Register(srv)
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("herd-guest: serving %d container(s) on %s", len(targets), *listen)
	return srv.Serve(l)
}
