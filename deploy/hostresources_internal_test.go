package deploy

import (
	"flag"
	"strings"
	"testing"
)

// TestHerdletCapacityIsItsHostLessTheReserve (PLT-452): by default a herdlet has no slot ceiling and reports its cores
// and memory less the reserve; a host whose memory cannot be read needs -slots; nonsense reserves are refused.
func TestHerdletCapacityIsItsHostLessTheReserve(t *testing.T) {
	h := &Herdlet{}
	fs := flag.NewFlagSet("herdlet", flag.ContinueOnError)
	h.HostFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if h.Slots != 0 || h.ReserveCPU != 1000 || h.ReserveMemoryMiB != 2048 || h.CPUOvercommit != 1 {
		t.Fatalf("defaults: -slots %d -reserve-cpu %d -reserve-memory %d -cpu-overcommit %v", h.Slots, h.ReserveCPU, h.ReserveMemoryMiB, h.CPUOvercommit)
	}
	cpu, mem, err := h.allocatable(hostTotals{cores: 8, memoryMiB: 15872})
	if err != nil || cpu != 7000 || mem != 13824 {
		t.Fatalf("8 cores, 15872 MiB: %dm, %d MiB, %v; want 7000m, 13824 MiB", cpu, mem, err)
	}
	h.CPUOvercommit = 2
	if cpu, _, _ := h.allocatable(hostTotals{cores: 8, memoryMiB: 15872}); cpu != 15000 {
		t.Errorf("-cpu-overcommit 2: %dm, want 15000m", cpu)
	}
	h.CPUOvercommit = 1

	if _, _, err := h.allocatable(hostTotals{cores: 8}); err == nil || !strings.Contains(err.Error(), "-slots") {
		t.Errorf("memory not read and no -slots: %v, want a refusal naming -slots", err)
	}
	h.Slots = 4
	if cpu, mem, err := h.allocatable(hostTotals{cores: 8}); err != nil || cpu != 0 || mem != 0 {
		t.Errorf("memory not read, -slots 4: %d, %d, %v; want nothing reported (placed by slots)", cpu, mem, err)
	}
	h.Slots = 0
	h.ReserveMemoryMiB = 20000
	if _, _, err := h.allocatable(hostTotals{cores: 8, memoryMiB: 15872}); err == nil {
		t.Error("a reserve bigger than the host: accepted")
	}
	h.ReserveMemoryMiB, h.Slots = 2048, -1
	if _, _, err := h.allocatable(hostTotals{cores: 8, memoryMiB: 15872}); err == nil {
		t.Error("-slots -1: accepted")
	}
}
