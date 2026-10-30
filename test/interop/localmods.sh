#!/bin/sh
# Writes a go.mod for the interop module whose replace directives point at copies of the repository's vendored
# modules, for boxes that cannot reach the Go module proxy. The grpc-go copy is the vendored version (go.mod's pin)
# labelled as the interop module's v1.84.0 requirement. Run from the repository root:
#
#   INTEROP_MODFILE=$(test/interop/localmods.sh /tmp/herd-interop-mods) GOPROXY=off test/interop/run.sh
set -e
out=${1:?usage: localmods.sh <dir>}
rm -rf "$out"
mkdir -p "$out"
mods="google.golang.org/grpc google.golang.org/protobuf golang.org/x/net golang.org/x/sys golang.org/x/text google.golang.org/genproto/googleapis/rpc"
mf="$out/interop.mod"
printf 'module github.com/everdict/herd/test/interop\n\ngo 1.25\n\nrequire (\n' >"$mf"
for m in $mods; do
	d="$out/$(echo "$m" | tr / _)"
	mkdir -p "$d"
	cp -r "vendor/$m/." "$d/"
	printf 'module %s\n\ngo 1.25\n' "$m" >"$d/go.mod"
	v=$(grep "^# $m " vendor/modules.txt | awk '{print $3}')
	[ "$m" = google.golang.org/grpc ] && v=v1.84.0
	printf '\t%s %s\n' "$m" "$v" >>"$mf"
done
printf ')\n' >>"$mf"
for m in $mods; do
	printf 'replace %s => %s\n' "$m" "$out/$(echo "$m" | tr / _)" >>"$mf"
done
: >"$out/interop.sum"
echo "$mf"
