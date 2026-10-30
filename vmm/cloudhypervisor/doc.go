// Package cloudhypervisor is herd's Ubuntu backend: Cloud Hypervisor (pinned v54.0) with virtiofsd (>= 1.11) for the
// volume share and hybrid vsock for control (spec R-D8). Snapshots are taken at MEMORY and restored eagerly:
// lazy userfaultfd restore is unavailable with vhost-user devices such as virtio-fs.
//
// Each VM is one cloud-hypervisor process driven over its REST API on a unix socket by a minimal in-repo client
// (ADR A-9), plus one virtiofsd per share started with --migration-mode=find-paths so that the snapshot carries the
// share. A host's restore family is microvm-ch-<arch>-<cpu-class>, the class derived from the CPUID words Cloud
// Hypervisor checks on restore (ADR A-8); a MEMORY snapshot of another family is refused before Cloud Hypervisor is
// asked. Delete answers gone only after the processes are observed exited and the VM's directory absent.
//
// Processes and the CPU are interfaces, so the package runs vmmtest.Run against an in-repo fake Cloud Hypervisor API
// server in every go test; a test gated by HERD_CH_REAL=1 runs the same contract on a KVM host.
package cloudhypervisor
