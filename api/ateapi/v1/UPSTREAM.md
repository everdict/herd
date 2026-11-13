# Upstream of the vendored Substrate API subset

| | |
|---|---|
| Repository | https://github.com/agent-substrate/substrate |
| File | `api/ateapi/v1/ateapi.proto` |
| Commit | `6d7b3f49f6154d52a1de46c94a1481868a5ed042` (2026-10-28) |
| sha256 of the file | `c51035499aadd1b961db5e9955de3ca63070ca1f3e2daa77ffc993ba4573ec44` |
| License | Apache License, Version 2.0 (Copyright 2026 Google LLC) |
| Copy | `testdata/upstream-ateapi.proto`, byte for byte |

`ateapi.proto` here is generated from that copy by `internal/cmd/protosubset` (`go generate ./api/...`): the
`Control` methods listed in `subset.txt`, every message and enum they reach, and the `resource`/`method` annotation
extensions. The protobuf package stays `substrate.ateapi.v1`, so a client of real Substrate calls herd with the same
full method names (`/substrate.ateapi.v1.Control/CreateActor`). Only `go_package` is rewritten.

Generated with protoc 36.2 (libprotoc) and protoc-gen-go v1.36.12; the runtime is google.golang.org/protobuf
v1.36.12, the version Substrate's own go.mod pins at this commit.

To move the pin: replace `testdata/upstream-ateapi.proto` with the new upstream file, update this table, run
`go generate ./api/...`, and let `go test ./api/... ./internal/...` say what changed.

herd's own fields on upstream messages (PLT-330, ADR 0001 A-34) are listed in `herd-additions.txt` and appended by
the same cut (`-additions`), each marked "herd addition" and numbered from 1000: today
`CreateActorTemplateRequest.image_pull_auth` and `image_plain_http_hosts` (DEFAUL-1600), and the same two on
`CreateActorRequest` (PLT-405). Generated with the same
protoc and protoc-gen-go as above (fetched from GitHub as build tools); they reproduce the previous `ateapi.pb.go`
byte for byte. `image_plain_http_hosts` was written into `ateapi.pb.go` where no protoc was: its raw descriptor is
the previous one with the field added (descriptorpb, proto.Marshal deterministic, quoted as protoc-gen-go quotes
it; the unchanged descriptor round-trips byte for byte), its struct field and getter in protoc-gen-go's shape.
`TestHerdAdditionsInTheDescriptor` holds the descriptor to `herd-additions.txt` and `ateapi.proto`; `go generate`
with protoc replaces the file. PLT-405's two fields were generated so (protoc 36.2 and protoc-gen-go v1.36.12, fetched
from GitHub), which also dropped two hand-written `Worker` comments that had been left on unrelated messages.

`ListActorTemplates` (DEFAUL-1613) was added to `subset.txt` and its two messages and method written into
`ateapi.pb.go` where no protoc was, the same way: the raw descriptor is the previous one with
`ListActorTemplatesRequest` and `ListActorTemplatesResponse` inserted after `GetActorTemplateRequest` and the method
after `GetActorTemplate`, where `ateapi.proto` has them; `goTypes`, `depIdxs` and the message indices are rewritten as
protoc-gen-go orders them (the same rewrite of the previous descriptor reproduces the previous file byte for byte),
the structs and getters in protoc-gen-go's shape. `go generate` with protoc replaces the file.

`ListTags` (DEFAUL-1613) was added the same way, its messages after `GetTagRequest` and the method after `GetTag`.

`Actor.provenance` and `ActorStatus.creator` (PLT-489, ADR 0001 A-60) were added to `herd-additions.txt` and generated
from it: `ateapi.proto` by `internal/cmd/protosubset`, `ateapi.pb.go` by protoc-gen-go v1.36.12 fed by a stand-in for
protoc (protocompile v0.14.1 compiling the same sources with standard source info, the CodeGeneratorRequest naming
libprotoc 7.36.2), where no protoc binary was. The same stand-in run on the previous `ateapi.proto` reproduced the
previous `ateapi.pb.go` byte for byte; `go generate` with protoc replaces the file.
