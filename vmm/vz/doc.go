// Package vz is herd's macOS host backend: Apple Virtualization.framework through Code-Hex/vz running the same arm64
// Linux guest, with virtio-fs volumes, native vsock and a file-handle network attachment into herd-egress (spec R-D8).
// A Mac declares VOLUMES unless it has shown it can save and restore machine state in its session (R-D9).
//
// R0 ships a stub that answers *vmm.NotImplementedError for every operation; the backend lands in R2/R7
// (PLT-114, PLT-118) and runs vmmtest.Run on a Mac.
package vz
