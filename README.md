# herd

herd is a microVM actor runtime that speaks a pinned subset of the [Substrate](https://github.com/agent-substrate/substrate)
API. It gives hosts that have no Kubernetes what Substrate gives a Kubernetes cluster: actors created from templates,
suspended and resumed, forked through tags, behind a per-actor egress gateway, deleted with verification.

Status: R1. `herd-api` serves the Control subset over gRPC with records, scheduler, suspend/resume/delete workflows,
snapshot store and static-key authn/authz, and `herdlet` drives a `vmm.VMM` over its outbound link; the conformance suite
runs them against the in-memory hypervisor fake. R3: `herd-router` routes clients to actors through the herdlets'
outbound tunnels and resumes suspended actors on request.

## What herd is

- **A Substrate-API server for clusters without Substrate.** `herd-api` serves a pinned subset of Substrate's `Control`
  service (`substrate.ateapi.v1`, same method names on the wire) plus a capability extension (`herd.ext.v1`), so one
  client integration drives real Substrate and herd alike.
- **Linux microVMs, on two hypervisors.** Every actor is a Linux microVM: Cloud Hypervisor on Ubuntu (x86_64, arm64,
  KVM), Apple Virtualization.framework on macOS hosts (Apple Silicon). Inside the VM, `herd-guest` runs the template's
  containers with crun and mounts its volumes from one virtio-fs share. One guest kernel and initramfs for both.
- **Honest about what each host can do.** A host declares how much a suspend keeps (MEMORY or VOLUMES) and which
  hosts can restore it (its restore family); herd never assumes more.
- **Three deployment modes, one code path:** all-in-one on one host, several hosts joined with `herdlet join` and no
  cluster tool, or a Nomad cluster.
- **A separate product.** Usable without Everdict. Go. Apache-2.0.

Components: `herd-api` (records, scheduler, suspend/resume, snapshot store, authn/authz), `herdlet` (node agent, dials
out), `herd-router` (resume-on-request ingress over node tunnels), `herd-egress` (default-deny, hostname rules,
credential injection), `herd-guest` (the in-VM agent).

## What herd is not

- **Not Substrate, and not all of its API.** Methods outside the pinned subset (workers, atespaces, access policies,
  revert, credential minting, the worker service) are not served; see `api/ateapi/v1/subset.txt`.
- **Not a container runtime on the host.** No docker, runsc or Seatbelt backends: no microVM, no actor. A host without
  KVM or Virtualization.framework cannot join.
- **Not a macOS guest runner.** herd runs Linux guests only. macOS-native work stays outside herd.
- **Not a live-migration system across platforms.** Memory snapshots never move between a Mac and an Ubuntu host, nor
  between Cloud Hypervisor hosts of different CPU feature classes; only volumes, image references and the container
  spec move.
- **Not a scheduler for arbitrary jobs.** On Nomad, Nomad supervises herd's processes; herd places actors.

## Layout

| Path | What |
|---|---|
| `docs/adr/0001-herd.md` | The decisions herd is built on |
| `api/ateapi/v1` | Vendored Substrate `Control` subset (proto + generated Go), pinned to upstream commit `6d7b3f49` |
| `api/herdext/v1` | herd's capability extension |
| `vmm` | The hypervisor boundary; `vmm/fake` (in-memory), `vmm/vmmtest` (the contract every backend keeps), `vmm/cloudhypervisor` (linux), `vmm/vz` (Virtualization.framework; binding darwin/arm64, `vmm/vz/vzfake` everywhere) |
| `guest` | Guest kernel configuration (one for arm64 and amd64), initramfs list, `build.sh` |
| `internal/cmd/protosubset` | Cuts the subset out of the upstream proto |
| `cmd/herd-api`, `apiserver` | herd-api: the Control subset, the Capabilities extension, workflows, `Reconcile` |
| `link`, `herdlet` | The herdlet's outbound link (three-valued outcomes) and the herdlet agent on a `vmm.VMM` |
| `sched` | Placement by cpu and memory (slots for an older herdlet), class, labels, warm VMs and restore family |
| `store` | Records with uid+version preconditions: `memstore`, `boltstore` (bbolt, all-in-one), `storetest` (the contract) |
| `snapstore` | Snapshot objects with digests, listed and swept of what no record names (A-58): `snapstore/disk`, `snapstore/s3`, `snapstore/bundlecache` |
| `auth` | Authn and per-space authz interfaces: SPIFFE SVIDs (X.509 over mTLS, JWT bearer) mapped by a policy file, HS256 JWTs as counted break-glass, the creator-scoped actor write rule (ADR 0001 A-60) |
| `guest/agent`, `guest/guestclient`, `cmd/herd-guest` | herd-guest: env-compatible process/file API on grpc-go, and its host-side client |
| `conformance` | The Control-subset conformance suite against herd-api with fake herdlets |
| `cmd/herd-router`, `router`, `tunnel` | herd-router: `ate-target-actor` ingress with per-space JWT, parking and resume-on-request, typed 503s and cut reasons; the herdlet's outbound tunnel (router side in `tunnel`, herdlet side in `herdlet/tunnel.go`) |
| `vmm/fakeguest` | A fake hypervisor whose VMs run the real herd-guest on unix sockets (the router's end-to-end test) |

## Developing

```sh
gofmt -l .        # prints nothing
go vet ./...
go test ./...
go build ./...
go generate ./api/...   # needs protoc and protoc-gen-go v1.36.12
```

## License

Apache License 2.0, see `LICENSE`. `api/ateapi/v1` contains a modified subset of Substrate's API definition,
Copyright 2026 Google LLC, under the same license; see `NOTICE` and `api/ateapi/v1/UPSTREAM.md`.
