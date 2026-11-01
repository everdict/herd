//go:build linux || darwin

package oci_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/oci"
)

// TestExecRunnerReturnsWhenADetachedChildKeepsStdio: `crun run --detach` exits while the container it started keeps
// the stdio it inherited. ExecRunner must return when the command exits, with its output, not wait for every holder
// of its stdout to close (with a pipe it hung until the container exited — herd-guest never served on a real VM).
func TestExecRunnerReturnsWhenADetachedChildKeepsStdio(t *testing.T) {
	script := filepath.Join(t.TempDir(), "detach.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho started\nsleep 30 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	out, err := oci.ExecRunner(ctx, nil, script)
	if err != nil {
		t.Fatalf("ExecRunner: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("ExecRunner returned after %v: it waited for the detached child's stdout", took)
	}
	if !strings.Contains(string(out), "started") {
		t.Fatalf("output %q, want the command's output", out)
	}
}
