//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReapedRunnerReturnsWhenADetachedChildKeepsStdio: PID 1's runner must return when `crun run --detach` exits,
// although the container keeps the stdio it inherited (with a pipe it waited until the container exited, so
// herd-guest never served its API on a real Cloud Hypervisor VM).
func TestReapedRunnerReturnsWhenADetachedChildKeepsStdio(t *testing.T) {
	script := filepath.Join(t.TempDir(), "detach.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho started\nsleep 30 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	out, err := reapedRunner(ctx, nil, script)
	if err != nil {
		t.Fatalf("reapedRunner: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("reapedRunner returned after %v: it waited for the detached child's stdout", took)
	}
	if !strings.Contains(string(out), "started") {
		t.Fatalf("output %q, want the command's output", out)
	}
}
