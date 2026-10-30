// Package vz is herd's macOS host backend: Apple Virtualization.framework through Code-Hex/vz running the same arm64
// Linux guest, with virtio-fs volumes, native vsock and a file-handle network attachment into herd-egress (spec R-D8).
//
// The backend's logic (VMM) builds on every platform against the Hypervisor interface; Native, the Code-Hex/vz
// binding, builds only on darwin/arm64 with cgo, and vzfake is the in-memory Hypervisor the tests run everywhere.
//
//   - Boot builds a VM from the kernel Image and initramfs with one virtio-fs device per share, a virtio-vsock device,
//     a virtio console logged to the VM's directory and, if configured, a file-handle NIC, and answers only once the
//     guest accepts a connection on its vsock port (herd-guest's 8080).
//   - Every Virtualization.framework call is bounded by the caller's context: one that does not answer makes the
//     operation answer unknown (presence) or fail (snapshots), never a guess (R-D6, ADR A-6).
//   - Snapshot at VOLUMES pauses the VM, flushes the shares to stable storage, records the snapshot durably and
//     resumes. MEMORY also saves the machine state, and is offered only on a Mac whose probe passed: Capabilities
//     declares MEMORY (family microvm-vz-arm64-<host>) only after validateSaveRestoreSupport answered true AND a
//     real save/restore round trip succeeded here; the R0 spike saw the first without the second (R-D9, ADR A-8).
//     It is MEMORY only while the console (Session) also has a user with the screen unlocked, and the probe is run
//     again on wake, after ProbeInterval and when the console is unlocked after a locked probe (ADR A-22).
//   - Delete answers gone only after the VM is observed stopped and its directory is observed absent.
//
// The binary that uses Native must be signed with the com.apple.security.virtualization entitlement (vz.entitlements;
// realtest.sh signs the test binary ad hoc and runs TestReal against a real guest).
package vz
