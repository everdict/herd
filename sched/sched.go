// Package sched places actors on workers. It places only by what herdlets reported — free cpu and memory (or, for a
// herdlet that reports none, free slots) of the actor's sandbox class, labels, warm VMs per template and, for a
// snapshot, the restore family its fidelity needs — and never assumes a capability (R-D9). When nothing fits it
// refuses with a typed reason per worker.
package sched

import (
	"fmt"
	"sort"
	"strings"

	"github.com/everdict/herd/vmm"
)

// Worker is one herdlet as the scheduler sees it.
type Worker struct {
	Name      string
	Class     string
	Labels    map[string]string
	Connected bool
	// Slots is the capacity the herdlet reported; Used counts the actors herd-api holds on it (including ones whose
	// release is not yet verified).
	Slots, Used int
	// MilliCPU and MemoryMiB are what the herdlet reported it can allocate to actors (after its operator's reserve);
	// 0 = not reported (a herdlet older than PLT-452), and such a worker is placed by Slots alone. UsedMilliCPU and
	// UsedMemoryMiB are what herd-api's actors on it are sized at (PLT-452).
	MilliCPU, MemoryMiB         int64
	UsedMilliCPU, UsedMemoryMiB int64
	// DefaultMilliCPU and DefaultMemoryMiB size an actor whose template sets no limits, and OverheadMiB is the memory
	// a VM holds beyond its guest's, as this herdlet boots them (Need).
	DefaultMilliCPU, DefaultMemoryMiB, OverheadMiB int64
	// Warm maps template uid → warm VMs.
	Warm               map[string]int
	SuspendMaxFidelity vmm.Fidelity
	RestoreFamily      string
}

// Snapshot is what a placement must be able to restore.
type Snapshot struct {
	Fidelity      vmm.Fidelity
	RestoreFamily string
}

// Request is one actor to place.
type Request struct {
	Class    string
	Template string
	Selector map[string]string
	// MilliCPU and MemoryMiB are the actor's template limits (PLT-452); 0 = the worker's default (Need).
	MilliCPU, MemoryMiB int64
	// Snapshot is nil for a cold boot.
	Snapshot *Snapshot
	// Exclude names workers that must not be chosen.
	Exclude map[string]bool
	// PreferFidelity puts the workers that keep the highest suspend fidelity first (a template's golden actor, whose
	// snapshot every later actor of the template restores).
	PreferFidelity bool
}

// NoFitError is a refused placement: why each worker was passed over.
type NoFitError struct {
	Class   string
	Reasons map[string]string
}

func (e *NoFitError) Error() string {
	if len(e.Reasons) == 0 {
		return fmt.Sprintf("sched: no worker of class %q is connected", e.Class)
	}
	names := make([]string, 0, len(e.Reasons))
	for n := range e.Reasons {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + ": " + e.Reasons[n]
	}
	return "sched: no worker fits: " + strings.Join(parts, "; ")
}

// Fits says why w cannot take r, or "" if it can.
func Fits(w Worker, r Request) string {
	switch {
	case r.Exclude[w.Name]:
		return "excluded"
	case !w.Connected:
		return "not connected"
	case w.Class != r.Class:
		return fmt.Sprintf("class %q, want %q", w.Class, r.Class)
	case (w.Slots > 0 || !w.ReportsResources()) && w.Used >= w.Slots:
		// A slot count binds a worker that reports no resources (an older herdlet), and is a ceiling where the
		// operator set one; otherwise the resources below decide (PLT-452).
		return fmt.Sprintf("no free slot (%d of %d used)", w.Used, w.Slots)
	}
	if why := w.resourceFit(r); why != "" {
		return why
	}
	for k, v := range r.Selector {
		if w.Labels[k] != v {
			return fmt.Sprintf("label %s=%q, want %q", k, w.Labels[k], v)
		}
	}
	if s := r.Snapshot; s != nil && s.Fidelity == vmm.FidelityMemory {
		if w.SuspendMaxFidelity < vmm.FidelityMemory || w.RestoreFamily != s.RestoreFamily {
			return fmt.Sprintf("MEMORY snapshot of family %q cannot restore in family %q", s.RestoreFamily, w.RestoreFamily)
		}
	}
	return ""
}

// ReportsResources says whether the herdlet reported what it can allocate (PLT-452).
func (w Worker) ReportsResources() bool { return w.MilliCPU > 0 || w.MemoryMiB > 0 }

// Need is what an actor of r's limits takes on w as w boots it: whole vCPUs (a VM gets ceil(millicores/1000)), the
// worker's default where the template sets no limit, and memory plus the VM's overhead. herd-api counts the actors it
// holds on a worker with the same function, so what is admitted and what is counted agree.
func (w Worker) Need(r Request) (milliCPU, memoryMiB int64) {
	milliCPU, memoryMiB = r.MilliCPU, r.MemoryMiB
	if milliCPU <= 0 {
		milliCPU = w.DefaultMilliCPU
	}
	if memoryMiB <= 0 {
		memoryMiB = w.DefaultMemoryMiB
	}
	return (milliCPU + 999) / 1000 * 1000, memoryMiB + w.OverheadMiB
}

// resourceFit says why r does not fit what is left on w, or "". An axis w does not report does not bound.
func (w Worker) resourceFit(r Request) string {
	cpu, mem := w.Need(r)
	if w.MemoryMiB > 0 && w.UsedMemoryMiB+mem > w.MemoryMiB {
		return fmt.Sprintf("memory: needs %d MiB, %d of %d MiB free", mem, max(0, w.MemoryMiB-w.UsedMemoryMiB), w.MemoryMiB)
	}
	if w.MilliCPU > 0 && w.UsedMilliCPU+cpu > w.MilliCPU {
		return fmt.Sprintf("cpu: needs %dm, %dm of %dm free", cpu, max(0, w.MilliCPU-w.UsedMilliCPU), w.MilliCPU)
	}
	return ""
}

// Place picks a worker for r: with PreferFidelity the one keeping the highest fidelity first, then one with a warm VM
// of the template, then the one with the most memory left (most free slots between workers reporting no resources),
// then by name. *NoFitError when none fits.
func Place(ws []Worker, r Request) (string, error) {
	var fit []Worker
	reasons := map[string]string{}
	for _, w := range ws {
		if why := Fits(w, r); why != "" {
			reasons[w.Name] = why
			continue
		}
		fit = append(fit, w)
	}
	if len(fit) == 0 {
		return "", &NoFitError{Class: r.Class, Reasons: reasons}
	}
	sort.Slice(fit, func(i, j int) bool {
		a, b := fit[i], fit[j]
		if r.PreferFidelity && a.SuspendMaxFidelity != b.SuspendMaxFidelity {
			return a.SuspendMaxFidelity > b.SuspendMaxFidelity
		}
		wa, wb := r.Snapshot == nil && a.Warm[r.Template] > 0, r.Snapshot == nil && b.Warm[r.Template] > 0
		if wa != wb {
			return wa
		}
		// Spread: the worker with the most memory left after the actor (PLT-452); between workers that report no
		// resources, the most free slots.
		if a.ReportsResources() && b.ReportsResources() {
			if fa, fb := a.MemoryMiB-a.UsedMemoryMiB, b.MemoryMiB-b.UsedMemoryMiB; fa != fb {
				return fa > fb
			}
		} else if fa, fb := a.Slots-a.Used, b.Slots-b.Used; fa != fb {
			return fa > fb
		}
		return a.Name < b.Name
	})
	return fit[0].Name, nil
}
