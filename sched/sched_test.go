package sched_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/everdict/herd/sched"
	"github.com/everdict/herd/vmm"
)

const famA, famB = "microvm-ch-amd64-tigerlake", "microvm-ch-amd64-lunarlake"

func worker(name, fam string, slots, used int) sched.Worker {
	return sched.Worker{Name: name, Class: "microvm", Connected: true, Slots: slots, Used: used, SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: fam}
}

func TestSlotsAreAHardLimit(t *testing.T) {
	ws := []sched.Worker{worker("h1", famA, 4, 4)}
	_, err := sched.Place(ws, sched.Request{Class: "microvm"})
	var nf *sched.NoFitError
	if !errors.As(err, &nf) || !strings.Contains(nf.Reasons["h1"], "no free slot") {
		t.Fatalf("5th actor on a 4-slot herdlet: %v, want NoFitError(no free slot)", err)
	}
}

func TestMemoryOnlyInItsFamily(t *testing.T) {
	ws := []sched.Worker{worker("b", famB, 4, 0), worker("a", famA, 4, 3)}
	mem := &sched.Snapshot{Fidelity: vmm.FidelityMemory, RestoreFamily: famA}
	if got, err := sched.Place(ws, sched.Request{Class: "microvm", Snapshot: mem}); err != nil || got != "a" {
		t.Errorf("MEMORY of %s placed on %q, %v; want a", famA, got, err)
	}
	ws[1].Used = 4
	var nf *sched.NoFitError
	if got, err := sched.Place(ws, sched.Request{Class: "microvm", Snapshot: mem}); !errors.As(err, &nf) {
		t.Errorf("MEMORY of %s with its family full placed on %q", famA, got)
	}
	vol := &sched.Snapshot{Fidelity: vmm.FidelityVolumes, RestoreFamily: vmm.PortableRestoreFamily}
	if got, err := sched.Place(ws, sched.Request{Class: "microvm", Snapshot: vol}); err != nil || got != "b" {
		t.Errorf("VOLUMES placed on %q, %v; want anywhere free (b)", got, err)
	}
	mac := sched.Worker{Name: "mac", Class: "microvm", Connected: true, Slots: 2, SuspendMaxFidelity: vmm.FidelityVolumes}
	if why := sched.Fits(mac, sched.Request{Class: "microvm", Snapshot: &sched.Snapshot{Fidelity: vmm.FidelityMemory}}); why == "" {
		t.Errorf("a VOLUMES-only host accepted a MEMORY restore with an empty family")
	}
}

func TestPreferenceAndFilters(t *testing.T) {
	a, b := worker("a", famA, 4, 0), worker("b", famA, 4, 2)
	b.Warm = map[string]int{"tpl": 1}
	c := worker("c", famA, 8, 0)
	c.Class = "other"
	d := worker("d", famA, 8, 0)
	d.Connected = false
	ws := []sched.Worker{a, b, c, d}
	if got, _ := sched.Place(ws, sched.Request{Class: "microvm", Template: "tpl"}); got != "b" {
		t.Errorf("warm worker not preferred: %q", got)
	}
	if got, _ := sched.Place(ws, sched.Request{Class: "microvm", Template: "x"}); got != "a" {
		t.Errorf("most free slots not preferred: %q", got)
	}
	if got, err := sched.Place(ws, sched.Request{Class: "microvm", Selector: map[string]string{"zone": "z1"}}); err == nil {
		t.Errorf("selector ignored: %q", got)
	}
	if got, _ := sched.Place(ws, sched.Request{Class: "microvm", Template: "tpl", Exclude: map[string]bool{"b": true}}); got != "a" {
		t.Errorf("exclude ignored: %q", got)
	}
}
