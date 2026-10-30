#!/bin/sh
# Builds herd's release archives: one tarball per platform with the static binaries the runbooks and dist/ install.
#
#   scripts/release.sh <out-dir>
#
# linux/amd64 and linux/arm64: herd, herd-api, herd-router, herd-egress, herdlet, all CGO_ENABLED=0 (static).
# darwin/arm64: herd-api, herd-router and herd-egress CGO_ENABLED=0; herd and herdlet drive Virtualization.framework
# through cgo, so they are built (and signed with vmm/vz/vz.entitlements) only when this script runs on a Mac.
# Set GUEST_DIR to a guest/build.sh output directory to add the guest kernel and initramfs (herd-guest-<arch>/{kernel,initrd.img}).
set -eu

OUT=${1:?usage: release.sh <out-dir>}
here=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

build() { # goos goarch cgo pkg out
	(cd "$here" && CGO_ENABLED=$3 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags='-s -w' -o "$5" "./cmd/$4")
}

for p in linux/amd64 linux/arm64 darwin/arm64; do
	goos=${p%/*} goarch=${p#*/}
	dir="$OUT/herd_${goos}_${goarch}"
	rm -rf "$dir" && mkdir -p "$dir"
	for c in herd-api herd-router herd-egress; do
		build "$goos" "$goarch" 0 "$c" "$dir/$c"
	done
	# herdlet and herd (all-in-one, which runs a herdlet) drive the hypervisor.
	for c in herd herdlet; do
		if [ "$goos" = linux ]; then
			build "$goos" "$goarch" 0 "$c" "$dir/$c"
		elif [ "$(uname -s)" = Darwin ]; then
			build "$goos" "$goarch" 1 "$c" "$dir/$c"
			codesign --force --entitlements "$here/vmm/vz/vz.entitlements" -s - "$dir/$c"
		else
			echo "release.sh: skipping darwin $c (Virtualization.framework needs cgo on a Mac)" >&2
		fi
	done
	if [ -n "${GUEST_DIR:-}" ] && [ -d "$GUEST_DIR/herd-guest-$goarch" ]; then
		cp "$GUEST_DIR/herd-guest-$goarch/kernel" "$GUEST_DIR/herd-guest-$goarch/initrd.img" "$dir/"
	fi
	cp -R "$here/dist" "$dir/dist"
	tar -C "$dir" -czf "$OUT/herd_${goos}_${goarch}.tar.gz" .
done
