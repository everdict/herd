# Upstream of the vendored agent-substrate/env guest API

| | |
|---|---|
| Repository | https://github.com/agent-substrate/env |
| File | `proto/ateenv/v1alpha/guest.proto` |
| Commit | `281e86085bb0b6d3090ff3fa82d722cf0d9f2f0b` (2026-10-27) |
| sha256 of the file | `e42206e1ce07ce515caae4754de58a29211cbf632cb2316440299f3856f7240d` |
| License | Apache License, Version 2.0 (Copyright 2026 Google LLC) |
| Copy | `guest.proto`, byte for byte |

`guest.proto` here is the upstream file unchanged. `guest.pb.go` is generated from it (`go generate ./api/...`)
with only the Go import path remapped by protoc's `M` option, so the protobuf package stays `ateenv.v1alpha` and full
method names are wire-identical to env's (`/ateenv.v1alpha.ProcessService/StartProcess`).

`testdata/upstream-guest.pb.go.txt` is upstream's own generated `proto/ateenv/v1alpha/guest.pb.go` at the same
commit (sha256 `6b1bba25f7040bb72857c36d6bfdcb59a0a9fa8c3ebd67e6c03221dec3349763`). `wire_test.go` compares the
serialized descriptor embedded there with the one herd generates: every message, field number, type, oneof, enum
value and method must be equal; only the file name and `go_package` may differ.

Generated with protoc 36.2 (libprotoc) and protoc-gen-go v1.36.12. env's gRPC service stubs are not used: herd-guest
serves the methods through `internal/grpcwire` (gRPC framing over net/http with unencrypted HTTP/2), so the
grpc-go module is not a dependency.
