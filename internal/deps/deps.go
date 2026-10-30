// Package deps pins modules that herd-api adopts next (PLT-113): grpc-go replaces internal/grpcwire (ADR A-10) and bbolt
// backs the all-in-one store (ADR A-11). They are vendored because delegate boxes cannot fetch modules; this
// file goes away once the packages that use them import them.
package deps

import (
	_ "go.etcd.io/bbolt"
	_ "google.golang.org/grpc"
)
