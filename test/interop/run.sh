#!/bin/sh
# Builds herd-guest from this checkout, serves a local target on a unix socket, and drives it with grpc-go and
# agent-substrate/env's own generated client (see README.md). Run from the repository root:
#
#   test/interop/run.sh                       # resolves grpc-go through the module proxy
#   INTEROP_MODFILE=/path/go.mod test/interop/run.sh   # a go.mod whose replace lines point at local sources
set -e
d=$(mktemp -d)
mkdir -p "$d/root"
go build -o "$d/herd-guest" ./cmd/herd-guest
(cd test/interop && GOFLAGS=-mod=mod go build ${INTEROP_MODFILE:+-modfile="$INTEROP_MODFILE"} -o "$d/interop" .)
"$d/herd-guest" serve -local "$d/root" -listen "unix://$d/g.sock" 2>"$d/log" &
pid=$!
i=0
while [ ! -S "$d/g.sock" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i + 1)); done
set +e
"$d/interop" "$d/g.sock"
rc=$?
kill $pid
exit $rc
