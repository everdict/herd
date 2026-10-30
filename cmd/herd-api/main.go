// Command herd-api serves the Substrate Control subset and herd's Capabilities extension, and accepts herdlets'
// outbound links. It never dials a herdlet.
//
//	herd-api -listen 127.0.0.1:7443 -key-file /etc/herd/jwt.key -snapshots /var/lib/herd/snapshots -store bolt:/var/lib/herd/herd.db
//	herd-api token -key-file /etc/herd/jwt.key -sub everdict -space team-a=write
//	herd-api token -key-file /etc/herd/jwt.key -sub node-1 -herdlet
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/boltstore"
	"github.com/everdict/herd/store/memstore"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		if err := token(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "herd-api token:", err)
			os.Exit(2)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil {
		log.Fatalf("herd-api: %v", err)
	}
}

func readKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("-key-file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k := []byte(strings.TrimSpace(string(b)))
	if len(k) < 32 {
		return nil, fmt.Errorf("%s: the key must be at least 32 bytes", path)
	}
	return k, nil
}

func openStore(spec string) (store.Store, error) {
	switch {
	case spec == "memory":
		return memstore.New(), nil
	case strings.HasPrefix(spec, "bolt:"):
		return boltstore.Open(strings.TrimPrefix(spec, "bolt:"), 5*time.Second)
	}
	return nil, fmt.Errorf("-store %q: want memory or bolt:<path>", spec)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("herd-api", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7443", "address to serve gRPC (cleartext HTTP/2) on")
	keyFile := fs.String("key-file", "", "HS256 signing key for bearer tokens (at least 32 bytes)")
	audience := fs.String("audience", "", "required JWT audience, if any")
	storeSpec := fs.String("store", "memory", "record store: memory or bolt:<path> (one herd-api per file)")
	snapDir := fs.String("snapshots", "", "snapshot store directory")
	callTimeout := fs.Duration("call-timeout", 30*time.Second, "how long to wait for a herdlet's answer")
	every := fs.Duration("reconcile", 10*time.Second, "how often to retry owed workflow steps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	if *snapDir == "" {
		return errors.New("-snapshots is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := openStore(*storeSpec)
	if err != nil {
		return err
	}
	defer st.Close()
	snaps, err := disk.New(*snapDir)
	if err != nil {
		return err
	}
	srv, err := apiserver.New(apiserver.Config{
		Store: st, Snapshots: snaps, Hub: link.NewHub(),
		Authn: auth.StaticKey{Key: key, Audience: *audience}, Authz: auth.SpaceRoles{}, CallTimeout: *callTimeout,
	})
	if err != nil {
		return err
	}
	g := grpc.NewServer()
	srv.Register(g)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if left, err := srv.Reconcile(ctx); err != nil {
					log.Printf("reconcile: %v", err)
				} else if left > 0 {
					log.Printf("reconcile: %d workflow steps still owed", left)
				}
			}
		}
	}()
	log.Printf("herd-api serving on %s", ln.Addr())
	go func() {
		<-ctx.Done()
		g.Stop()
	}()
	err = g.Serve(ln)
	srv.Wait()
	return err
}

func token(args []string) error {
	fs := flag.NewFlagSet("herd-api token", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "HS256 signing key")
	sub := fs.String("sub", "", "subject")
	aud := fs.String("audience", "", "audience")
	ttl := fs.Duration("ttl", 24*time.Hour, "lifetime")
	herdlet := fs.Bool("herdlet", false, "a herdlet credential (may open the link, nothing else)")
	var spaces []string
	fs.Func("space", "atespace=read|write (repeatable; * for every space)", func(v string) error {
		spaces = append(spaces, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	c := auth.Claims{Subject: *sub, Audience: *aud, ExpiresAt: time.Now().Add(*ttl).Unix(), Herdlet: *herdlet, Spaces: map[string]auth.Role{}}
	for _, s := range spaces {
		name, role, ok := strings.Cut(s, "=")
		if !ok || (role != string(auth.RoleRead) && role != string(auth.RoleWrite)) {
			return fmt.Errorf("-space %q: want atespace=read|write", s)
		}
		c.Spaces[name] = auth.Role(role)
	}
	tok, err := auth.Sign(key, c)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
