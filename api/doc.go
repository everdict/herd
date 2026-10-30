// Package api holds herd's wire API: the pinned subset of Substrate's Control service (ateapi/v1, package
// substrate.ateapi.v1, so full method names match real Substrate) and herd's capability extension (herdext/v1).
//
// Regenerating needs protoc and protoc-gen-go v1.36.12 on PATH.
package api

//go:generate go run ../internal/cmd/protosubset -upstream ateapi/v1/testdata/upstream-ateapi.proto -methods ateapi/v1/subset.txt -source "github.com/agent-substrate/substrate api/ateapi/v1/ateapi.proto at commit 6d7b3f49f6154d52a1de46c94a1481868a5ed042 (Apache License, Version 2.0)" -go-package github.com/everdict/herd/api/ateapi/v1;ateapi -out ateapi/v1/ateapi.proto
//go:generate protoc -I . --go_out=. --go_opt=paths=source_relative ateapi/v1/ateapi.proto herdext/v1/capabilities.proto
//go:generate protoc -I . --go_out=. --go_opt=paths=source_relative herdext/v1/files.proto
//go:generate protoc -I . --go_out=. --go_opt=paths=source_relative --go_opt=Mateenv/v1alpha/guest.proto=github.com/everdict/herd/api/ateenv/v1alpha;ateenvv1alpha ateenv/v1alpha/guest.proto
