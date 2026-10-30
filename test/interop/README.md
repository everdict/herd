# grpc-go interop check for herd-guest

A separate module (not part of `go test ./...`: env's generated code registers the same proto names as herd's
`api/ateenv/v1alpha`, so one binary cannot link both): `main.go` drives a running
herd-guest with grpc-go v1.84.0 (env's pin) through agent-substrate/env's own generated client —
`envclient/guest.pb.go` and `envclient/guest_grpc.pb.go` are env's `proto/ateenv/v1alpha` files at commit
`281e86085bb0b6d3090ff3fa82d722cf0d9f2f0b`, unmodified (Apache License 2.0, Copyright 2026 Google LLC). It checks
StartProcess, client-streamed stdin, streamed stdout/stderr and exit, GetProcess, SignalProcess, status codes, and a
WriteFile/ReadFile round trip.

`test/interop/run.sh` from the repository root builds both and runs the check; exit 0 means every check passed.
On a box that cannot reach the Go module proxy, point `INTEROP_MODFILE` at a copy of `go.mod` with `replace`
directives to local sources of grpc-go and its dependencies (unused ones may be empty stub modules).

On this repository's build boxes: `INTEROP_MODFILE=$(test/interop/localmods.sh /tmp/herd-interop-mods) GOPROXY=off
test/interop/run.sh` builds that `go.mod` from `vendor/` (grpc-go is then the vendored v1.83.2, the version herd-guest
itself serves on).
