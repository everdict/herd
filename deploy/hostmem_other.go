//go:build !linux && !darwin

package deploy

// hostMemoryMiB is not read on this OS: the herdlet is placed by -slots.
func hostMemoryMiB() int64 { return 0 }
