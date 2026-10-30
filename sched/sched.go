// Package sched places actors on workers. It places only by what herdlets reported — free slots of the actor's
// sandbox class, labels, warm VMs per template and, for a snapshot, the restore family its fidelity needs — and never
// assumes a capability (R-D9). When nothing fits it refuses with a typed reason per worker.
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
	// Snapshot is nil for a cold boot.
	Snapshot *Snapshot
	// Exclude names workers that must not be chosen.
	Exclude map[string]bool
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
	case w.Used >= w.Slots:
		return fmt.Sprintf("no free slot (%d of %d used)", w.Used, w.Slots)
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

// Place picks a worker for r: one with a warm VM of the template first, then the one with most free slots, then by
// name. *NoFitError when none fits.
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
		wa, wb := r.Snapshot == nil && a.Warm[r.Template] > 0, r.Snapshot == nil && b.Warm[r.Template] > 0
		if wa != wb {
			return wa
		}
		if fa, fb := a.Slots-a.Used, b.Slots-b.Used; fa != fb {
			return fa > fb
		}
		return a.Name < b.Name
	})
	return fit[0].Name, nil
}
