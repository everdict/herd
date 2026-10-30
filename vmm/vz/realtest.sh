#!/bin/sh
# Runs vmm/vz's real-host test (TestReal) on an Apple Silicon Mac: builds internal/vzguest as a static linux/arm64
# /init, packs it into an initramfs, builds the test binary, ad-hoc signs it with com.apple.security.virtualization
# and boots the guest kernel given as $1 (the arm64 Image, e.g. guest/build.sh's dist/Image).
#
#   vmm/vz/realtest.sh /path/to/Image [extra go test flags]
set -eu
kernel=${1:?usage: realtest.sh /path/to/arm64/Image [go test flags]}
shift
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${HERD_VZ_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/herd-vz-real.XXXXXX")}

cd "$root"
mkdir -p "$out/initrd/mnt/volumes" "$out/initrd/proc" "$out/initrd/sys" "$out/initrd/dev"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$out/initrd/init" ./vmm/vz/internal/vzguest
(cd "$out/initrd" && find . | cpio -o -H newc 2>/dev/null | gzip -9 >"$out/initrd.img")

go test -c -o "$out/vz.test" ./vmm/vz
codesign --force --sign - --entitlements "$here/vz.entitlements" "$out/vz.test"
codesign -d --entitlements - "$out/vz.test"

HERD_VZ_REAL=1 HERD_VZ_KERNEL=$kernel HERD_VZ_INITRD=$out/initrd.img "$out/vz.test" -test.run '^TestReal$' -test.v "$@"
