// Package cloudhypervisor is herd's Ubuntu backend: Cloud Hypervisor (pinned v54.0) with virtiofsd (>= 1.11) for the
// volume share and hybrid vsock for control (spec R-D8). Snapshots are taken at MEMORY and restored eagerly:
// lazy userfaultfd restore is unavailable with vhost-user devices such as virtio-fs.
//
// R0 ships a stub that answers *vmm.NotImplementedError for every operation; the backend lands in R2 (PLT-114) and
// runs vmmtest.Run on a KVM host.
package cloudhypervisor
