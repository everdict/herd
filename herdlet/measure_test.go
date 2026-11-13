package herdlet

import (
	"bufio"
	"crypto/rand"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/bundlecache"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// sourceText is the Go standard library's source, as page cache of text files holds it.
func sourceText(t *testing.T) []byte {
	t.Helper()
	var b []byte
	err := filepath.WalkDir(filepath.Join(runtime.GOROOT(), "src"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(b) > 64<<20 {
			return err
		}
		if d.Type().IsRegular() && strings.HasSuffix(p, ".go") {
			f, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b = append(b, f...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMeasureSuspend (PLT-591) measures one MEMORY suspend of a VM through herdlet, from the VMM's files to the object
// in an S3 endpoint (s3test, in this process) that takes each request's body at HERD_MEASURE_STREAM_MIBS MiB/s (default
// 50, the node-to-control-host rate PLT-591 read over one ssh stream; a model, not the tailnet): the seconds, the
// object's bytes, the bytes the endpoint received and the bytes left on the node. The VM's memory file is
// HERD_MEASURE_MIB MiB (default 4096, a 4096 MiB limit) of which HERD_MEASURE_USED_MIB (default 768) is used, in 2 MiB
// runs spread over it, half Go source text and half random bytes; the rest is zero. It runs only when HERD_MEASURE is
// set; each line it logs starts with MEASURE.
func TestMeasureSuspend(t *testing.T) {
	if os.Getenv("HERD_MEASURE") == "" {
		t.Skip("HERD_MEASURE is not set")
	}
	env := func(name string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
			return v
		}
		return def
	}
	size, used, rate := int64(env("HERD_MEASURE_MIB", 4096))<<20, int64(env("HERD_MEASURE_USED_MIB", 768))<<20, int64(env("HERD_MEASURE_STREAM_MIBS", 50))<<20
	text := sourceText(t)
	write := func(p string, size int64) error {
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		defer f.Close()
		w := bufio.NewWriterSize(f, 1<<20)
		const run = 2 << 20
		every := max(1, size/used) // one used run in every this many
		zero, data := make([]byte, run), make([]byte, run)
		for i, off := int64(0), int64(0); off < size; i, off = i+1, off+run {
			b := zero
			if i%every == 0 {
				b = data
				if (i/every)%2 == 0 {
					copy(data, text[(off/run*run)%int64(len(text)-run):])
				} else if _, err := rand.Read(data); err != nil {
					return err
				}
			}
			if _, err := w.Write(b[:min(run, size-off)]); err != nil {
				return err
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		return f.Sync()
	}
	ctx := t.Context()
	dir := t.TempDir()
	caps := vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-test"}
	v := &diskVMM{Fake: fake.New(caps, nil), dir: filepath.Join(dir, "vmm-snapshots"), memory: size, write: write}
	reg := imagetest.NewRegistry()
	reg.Push("team/agent", "1", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Config: image.Config{Cmd: []string{"/bin/run"}}, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "run", Mode: 0o755})}})
	m, err := machine.New(machine.Config{VMM: v, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
		Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		t.Fatal(err)
	}
	creds := s3.Credentials{AccessKeyID: "herd-test", SecretAccessKey: "herd-test-secret"}
	srv := s3test.New("us-east-1", creds)
	srv.SetWriteRate(rate)
	ts := s3test.Start(t, srv)
	snaps, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := bundlecache.New(filepath.Join(dir, "bundles"), 64<<30)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Worker: "measure", VMM: v, Machines: m, Snapshots: snaps, Bundles: bundles}
	tpl := link.Template{UID: "tpl-1", Containers: []link.Container{{Name: "agent", Image: "registry.example/team/agent:1"}}}
	if o := a.doMachine(ctx, link.Work{Op: link.OpBoot, VM: "vm-1", Spec: &tpl}); o.Result != link.ResultDone {
		t.Fatalf("boot: %+v", o)
	}
	start := time.Now()
	o := a.doMachine(ctx, link.Work{Op: link.OpSnapshot, VM: "vm-1", Fidelity: vmm.FidelityMemory, Cause: link.CauseSuspend, SnapshotKey: "s/a1/1"})
	took := time.Since(start)
	if o.Result != link.ResultDone {
		t.Fatalf("snapshot: %+v", o)
	}
	obj, err := snaps.Stat(ctx, o.Snapshot.Key)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURE memory_bytes=%d used_bytes=%d stream_mibs=%d suspend_s=%.2f object_bytes=%d endpoint_received_bytes=%d node_vmm_bytes=%d node_cache_bytes=%d",
		size, used, rate>>20, took.Seconds(), obj.Size, srv.Received(), diskBytes(t, v.dir), diskBytes(t, filepath.Join(dir, "bundles")))
	// The resume on this node, from the bundle cache, after herd-api deleted the suspended VM.
	if o := a.doMachine(ctx, link.Work{Op: link.OpDelete, VM: "vm-1", Cause: link.CauseSuspend}); o.Result != link.ResultDone {
		t.Fatalf("delete: %+v", o)
	}
	start = time.Now()
	if o := a.doMachine(ctx, link.Work{Op: link.OpRestore, VM: "vm-1", Snapshot: o.Snapshot}); o.Result != link.ResultDone {
		t.Fatalf("restore: %+v", o)
	}
	t.Logf("MEASURE resume_here_s=%.2f", time.Since(start).Seconds())
}
