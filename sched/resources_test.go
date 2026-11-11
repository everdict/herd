package sched_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/everdict/herd/sched"
	"github.com/everdict/herd/vmm"
)

// PLT-452: a worker that reports its allocatable cpu and memory is placed by what is left of them, not by a slot count.

// sized is an 8-core, 15 GiB worker reporting resources and no slot ceiling, holding `held` actors of 2 cpu / 4 GiB.
func sized(name string, held int) sched.Worker {
	return sched.Worker{
		Name: name, Class: "microvm", Connected: true, SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: famA,
		MilliCPU: 8000, MemoryMiB: 15 * 1024, Used: held,
		UsedMilliCPU: int64(held) * 2000, UsedMemoryMiB: int64(held) * 4096,
	}
}

var box = sched.Request{Class: "microvm", MilliCPU: 2000, MemoryMiB: 4096}

func TestAFourthBoxGoesToTheEmptyNodeNotRefusedByASlotCount(t *testing.T) {
	ws := []sched.Worker{sized("a", 3), sized("b", 0)}
	got, err := sched.Place(ws, box)
	if err != nil || got != "b" {
		t.Fatalf("4th 2cpu/4GiB box with a holding 3 and b empty: placed on %q, %v; want b", got, err)
	}
	// Under the slot rule both workers say Slots 0 and nothing would fit: the resources are what admit it.
}

func TestABoxBiggerThanWhatIsLeftIsRefusedNamingMemory(t *testing.T) {
	w := sized("a", 0)
	w.UsedMemoryMiB = 9 * 1024 // 6 GiB left
	_, err := sched.Place([]sched.Worker{w}, sched.Request{Class: "microvm", MilliCPU: 1000, MemoryMiB: 10 * 1024})
	var nf *sched.NoFitError
	if !errors.As(err, &nf) || !strings.Contains(nf.Reasons["a"], "memory") {
		t.Fatalf("10 GiB box on a node with 6 GiB left: %v, want NoFitError naming memory", err)
	}
}

func TestCPUBoundsToo(t *testing.T) {
	w := sized("a", 0)
	w.UsedMilliCPU = 7000
	_, err := sched.Place([]sched.Worker{w}, box)
	var nf *sched.NoFitError
	if !errors.As(err, &nf) || !strings.Contains(nf.Reasons["a"], "cpu") {
		t.Fatalf("2-cpu box on a node with 1 cpu left: %v, want NoFitError naming cpu", err)
	}
}

func TestTwoBoxesBackToBackLandOnDifferentNodes(t *testing.T) {
	ws := []sched.Worker{sized("a", 0), sized("b", 0)}
	first, err := sched.Place(ws, box)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ws {
		if ws[i].Name == first {
			ws[i].Used++
			ws[i].UsedMilliCPU += box.MilliCPU
			ws[i].UsedMemoryMiB += box.MemoryMiB
		}
	}
	second, err := sched.Place(ws, box)
	if err != nil || second == first {
		t.Fatalf("second box placed on %q (first on %q), %v; want the other node", second, first, err)
	}
}

func TestTheNodeWithMoreFreeMemoryWins(t *testing.T) {
	a, b := sized("a", 0), sized("b", 0)
	a.MemoryMiB = 30 * 1024 // a bigger node with one actor still has more left
	a.Used, a.UsedMilliCPU, a.UsedMemoryMiB = 1, 2000, 4096
	if got, err := sched.Place([]sched.Worker{b, a}, box); err != nil || got != "a" {
		t.Fatalf("placed on %q, %v; want a (26 GiB free beats 15)", got, err)
	}
}

func TestASlotCeilingStillBindsWhenSet(t *testing.T) {
	w := sized("a", 2)
	w.Slots = 2
	_, err := sched.Place([]sched.Worker{w}, box)
	var nf *sched.NoFitError
	if !errors.As(err, &nf) || !strings.Contains(nf.Reasons["a"], "no free slot") {
		t.Fatalf("a worker at its -slots 2 ceiling with resources left: %v, want NoFitError(no free slot)", err)
	}
}

func TestAWorkerReportingNoResourcesKeepsTheSlotRule(t *testing.T) {
	old := worker("old", famA, 4, 1) // an older herdlet: Slots only
	if got, err := sched.Place([]sched.Worker{old}, box); err != nil || got != "old" {
		t.Fatalf("older herdlet with a free slot: %q, %v; want old", got, err)
	}
	old.Used = 4
	var nf *sched.NoFitError
	if _, err := sched.Place([]sched.Worker{old}, box); !errors.As(err, &nf) {
		t.Fatalf("older herdlet at 4 of 4: %v, want NoFitError", err)
	}
}
