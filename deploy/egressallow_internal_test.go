package deploy

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// TestEgressAllowPrefixFlag (criteria allow-flag, visible): herd, herd-egress and herdlet take a repeatable
// -egress-allow-prefix; herd-egress and herdlet refuse at start one that would open more than written, and log a set
// one loudly.
func TestEgressAllowPrefixFlag(t *testing.T) {
	args := []string{"-egress-allow-prefix", "100.113.143.93/32", "-egress-allow-prefix", "10.20.30.40/32"}
	want := []string{"100.113.143.93/32", "10.20.30.40/32"}
	var e Egress
	var h Herdlet
	var a AllInOne
	for name, reg := range map[string]func(*flag.FlagSet){"herd-egress": e.Flags, "herdlet": h.RunFlags, "herd": a.Flags} {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		reg(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, got := range map[string][]string{"herd-egress": e.AllowPrefixes, "herdlet": h.EgressAllow, "herd": a.Herdlet.EgressAllow} {
		if !slices.Equal(got, want) {
			t.Errorf("%s: -egress-allow-prefix gave %v, want %v", name, got, want)
		}
	}

	for _, broad := range []string{"100.64.0.0/10", "100.113.143.93/10", "0.0.0.0/0"} {
		eg := Egress{Bindings: "/nonexistent/bindings.json", AllowPrefixes: list{broad}}
		if err := eg.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "-egress-allow-prefix") {
			t.Errorf("herd-egress -egress-allow-prefix %s: Run = %v, want a refusal at start", broad, err)
		}
		hl := Herdlet{VMM: fake.New(vmm.Capabilities{}, fake.NewStore()), EgressAllow: list{broad}}
		if err := hl.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "-egress-allow-prefix") {
			t.Errorf("herdlet -egress-allow-prefix %s: Run = %v, want a refusal at start", broad, err)
		}
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	if err := labAllow("herd-egress", want); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "herd-egress: WARNING: LAB EGRESS ALLOWANCE -egress-allow-prefix 100.113.143.93/32,10.20.30.40/32") {
		t.Errorf("start log %q does not name the allowance", got)
	}
}
