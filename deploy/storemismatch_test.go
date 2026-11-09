package deploy_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// freeAddr is a loopback address no one listens on now, so that an all-in-one restarted on it serves where it did,
// as `herd` does at its fixed -listen.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// runAllInOne runs a until stop is called, once it is ready.
func runAllInOne(t *testing.T, a *deploy.AllInOne) (deploy.Addrs, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan deploy.Addrs, 1)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, func(ad deploy.Addrs) { ready <- ad }) }()
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("all-in-one: %v", err)
		}
	}
	t.Cleanup(stop)
	select {
	case ad := <-ready:
		return ad, stop
	case err := <-done:
		stopped = true
		t.Fatalf("all-in-one stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("all-in-one not ready")
	}
	return deploy.Addrs{}, stop
}

// suspendOne creates an actor of a cold template and suspends it synchronously: herd-api answers SUSPENDED only once
// it proved the herdlet's snapshot object in its own store.
func suspendOne(t *testing.T, addr string, key []byte) error {
	t.Helper()
	ctl := writeToken(t, key)
	waitWorkers(t, addr, ctl, 1)
	c := conformance.NewClient(addr, ctl)
	defer c.Close()
	cctx := ctxT(t)
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, GoldenSnapshotConfig: coldBoot},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	if _, err := c.CreateActor(cctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: space, Name: "a1"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "agent"},
	}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	_, err := c.SuspendActor(cctx, &ateapi.SuspendActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: "a1"},
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}})
	return err
}

// TestAllInOneRestartedWithAnotherStoreUsesIt (PLT-382, criterion store-mismatch-not-silent): `herd` restarted on the
// same data directory with another snapshot store than its last run (-s3 added, or dropped) keeps its snapshots where
// herd-api keeps them: the herdlet's join grant in <data>/herdlet/join.json, which names the old store, is replaced,
// and a synchronous suspend succeeds with its object in the store herd-api uses. No S3 secret is logged. Its
// counterexample is herd at 4510bce, which reused the grant because the worker and herd-api's address were the same:
// the herdlet wrote to the old store, herd-api proved nothing in its own, and the suspend answered "not durable"; on
// nahyunii-pc every golden build did the same.
func TestAllInOneRestartedWithAnotherStoreUsesIt(t *testing.T) {
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret-382"}
	for _, tc := range []struct {
		name          string
		first, second bool // with -s3
	}{
		{"disk then s3", false, true},
		{"s3 then disk", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := s3test.New("us-east-1", creds)
			ts := s3test.Start(t, srv)
			t.Setenv("AWS_ACCESS_KEY_ID", creds.AccessKeyID)
			t.Setenv("AWS_SECRET_ACCESS_KEY", creds.SecretAccessKey)
			data := t.TempDir()
			listen, router, tunnel := freeAddr(t), freeAddr(t), freeAddr(t)
			herd := func(withS3 bool) *deploy.AllInOne {
				a := &deploy.AllInOne{Data: data, Listen: listen, RouterListen: router, TunnelListen: tunnel,
					Herdlet: herdletOn(t, "aio", nil)}
				if withS3 {
					a.S3, a.S3Endpoint = "s3://herd/snapshots", ts.URL
				}
				return a
			}
			_, stop := runAllInOne(t, herd(tc.first))
			stop()
			if _, err := os.Stat(deploy.StatePath(filepath.Join(data, "herdlet"))); err != nil {
				t.Fatalf("the first run left no join grant: %v", err)
			}

			var logs syncBuffer
			out := log.Writer()
			log.SetOutput(io.MultiWriter(&logs, out))
			t.Cleanup(func() { log.SetOutput(out) })
			addrs, _ := runAllInOne(t, herd(tc.second))
			key, err := deploy.ReadKey(filepath.Join(data, "jwt.key"))
			if err != nil {
				t.Fatal(err)
			}
			if err := suspendOne(t, addrs.API, key); err != nil {
				t.Errorf("SuspendActor after the restart: %v", err)
			}
			inBucket := len(srv.Keys()) > 0
			if inBucket != tc.second {
				t.Errorf("the bucket holds %v; want a snapshot there exactly when herd-api runs with -s3 (%v)", srv.Keys(), tc.second)
			}
			if strings.Contains(logs.String(), creds.SecretAccessKey) {
				t.Errorf("the log holds the S3 secret")
			}
		})
	}
}

// TestJoinedHerdletWithAnotherStoreRefusesToLink (PLT-382, criterion store-mismatch-not-silent): a herdlet started on
// a state directory whose join grant names another snapshot store than the one herd-api uses now (herd-api restarted
// with -s3 added or dropped) does not run on it: herd-api refuses its link naming both stores and the fix, and the
// herdlet stops with that error. Neither the error nor the log holds the S3 secret. Its counterexample is herd at
// 4510bce: the herdlet linked and served, writing every snapshot where herd-api never looks.
func TestJoinedHerdletWithAnotherStoreRefusesToLink(t *testing.T) {
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret-382"}
	for _, tc := range []struct {
		name    string
		apiS3   bool // herd-api runs with -s3; the grant names the other store
		grantS3 bool
	}{
		{"grant disk, herd-api s3", true, false},
		{"grant s3, herd-api disk", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var addr string
			var key []byte
			var bucket *s3.Config
			if tc.apiS3 {
				_, _, addr, key, _ = s3HerdAPI(t)
			} else {
				a := startAPI(t)
				addr, key = a.addr, a.key
				ts := s3test.Start(t, s3test.New("us-east-1", creds))
				bucket = &s3.Config{Endpoint: ts.URL, Region: "us-east-1", Bucket: "herd", Prefix: "snapshots",
					AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}
			}
			var logs syncBuffer
			out := log.Writer()
			log.SetOutput(io.MultiWriter(&logs, out))
			t.Cleanup(func() { log.SetOutput(out) })

			h := herdletOn(t, "node-1", nil)
			h.Snapshots = filepath.Join(t.TempDir(), "snapshots")
			cred, err := auth.Sign(key, auth.Claims{Subject: "node-1", ExpiresAt: time.Now().Add(time.Hour).Unix(), Herdlet: true})
			if err != nil {
				t.Fatal(err)
			}
			st := deploy.State{API: addr, Grant: join.Grant{Worker: "node-1", Credential: cred, Snapshots: bucket}}
			b, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(h.StateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(deploy.StatePath(h.StateDir), b, 0o600); err != nil {
				t.Fatal(err)
			}
			done := runHerdlet(t, h)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the herdlet stopped with no error")
				}
				msg := err.Error()
				for _, want := range []string{"snapshot", "join.json", "join again"} {
					if !strings.Contains(msg, want) {
						t.Errorf("the herdlet stopped with %q, which does not say %q", msg, want)
					}
				}
				// Both herd-api's (s3HerdAPI) and this test's secret begin so.
				if strings.Contains(msg, "herd-secret") || strings.Contains(logs.String(), "herd-secret") {
					t.Errorf("the error or the log holds the S3 secret")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the herdlet is still running 10 s after it started on a grant naming another snapshot store than herd-api's")
			}
		})
	}
}
