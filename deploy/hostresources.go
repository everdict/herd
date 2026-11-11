package deploy

import (
	"fmt"
	"runtime"
)

// hostTotals is what a host has: its cores and its memory in MiB (0: not read).
type hostTotals struct {
	cores     int
	memoryMiB int64
}

// hostResources reads this host's cores and memory.
func hostResources() hostTotals {
	return hostTotals{cores: runtime.NumCPU(), memoryMiB: hostMemoryMiB()}
}

// allocatable is what the herdlet reports it can give VMs (PLT-452): cores × -cpu-overcommit less -reserve-cpu, and
// memory less -reserve-memory. A host whose memory is not read reports none, and is then placed by -slots, which must be
// set: a herdlet with neither would be offered nothing or everything.
func (h *Herdlet) allocatable(t hostTotals) (milliCPU, memoryMiB int64, err error) {
	switch {
	case h.Slots < 0:
		return 0, 0, fmt.Errorf("-slots %d", h.Slots)
	case h.ReserveCPU < 0 || h.ReserveMemoryMiB < 0:
		return 0, 0, fmt.Errorf("-reserve-cpu %d, -reserve-memory %d: a reserve is not negative", h.ReserveCPU, h.ReserveMemoryMiB)
	case h.CPUOvercommit < 0:
		return 0, 0, fmt.Errorf("-cpu-overcommit %v: must be positive", h.CPUOvercommit)
	}
	overcommit := h.CPUOvercommit
	if overcommit == 0 {
		overcommit = 1 // a Herdlet built without its flags
	}
	if t.memoryMiB <= 0 {
		if h.Slots == 0 {
			return 0, 0, fmt.Errorf("this host's memory cannot be read, so its capacity is -slots, which is 0: set -slots")
		}
		return 0, 0, nil
	}
	milliCPU = int64(float64(t.cores)*1000*overcommit) - h.ReserveCPU
	memoryMiB = t.memoryMiB - h.ReserveMemoryMiB
	if milliCPU <= 0 || memoryMiB <= 0 {
		return 0, 0, fmt.Errorf("the reserves leave VMs nothing: %d cores × %v less %dm, %d MiB less %d MiB",
			t.cores, overcommit, h.ReserveCPU, t.memoryMiB, h.ReserveMemoryMiB)
	}
	return milliCPU, memoryMiB, nil
}
