#!/bin/sh
# Builds the herd guest kernel and initramfs from the one shared configuration (spec R-D8), for one architecture
# or for both: `all` builds amd64 and arm64 into the same <out-dir> (herd-guest-<arch>), which
# scripts/release.sh packs per platform (GUEST_DIR); herdlet doctor checks a host boots its own (guest.KernelArch).
#
#   BUSYBOX_<arch>=... CRUN_<arch>=... guest/build.sh <amd64|arm64|all> <out-dir>
#   guest/build.sh --plan <amd64|arm64|all> <out-dir>     (print what would be built, build nothing)
#   BUSYBOX=... CRUN=... guest/build.sh --initrd <amd64|arm64|all> <out-dir>   (herd-guest and initrd.img only, no kernel)
#
# BUSYBOX and CRUN (or BUSYBOX_<arch> and CRUN_<arch>, which win) are static binaries for the target architecture.
# herd-guest is built from this checkout (CGO_ENABLED=0 GOOS=linux GOARCH=<arch>) unless HERD_GUEST(_<arch>) names
# a binary. Needs git, make, flex, bison, bc, libelf headers and a C toolchain for the target (CROSS_COMPILE_<arch>
# or CROSS_COMPILE when building for the other architecture).
# Output: <out-dir>/herd-guest-<arch>/{kernel,initrd.img,config}.
set -eu

PLAN= INITRD=
case "${1:-}" in
--plan)
	PLAN=1
	shift
	;;
--initrd)
	INITRD=1
	shift
	;;
esac
ARCH=${1:?usage: build.sh [--plan|--initrd] <amd64|arm64|all> <out-dir>}
OUT=${2:?usage: build.sh [--plan|--initrd] <amd64|arm64|all> <out-dir>}
here=$(cd "$(dirname "$0")" && pwd)

if [ "$ARCH" = all ]; then
	for a in amd64 arm64; do
		sh "$0" ${PLAN:+--plan} ${INITRD:+--initrd} "$a" "$OUT"
	done
	exit 0
fi
. "$here/kernel/source.env"

case "$ARCH" in
amd64)
	KARCH=x86_64
	TARGET=bzImage
	IMAGE=arch/x86/boot/bzImage
	;;
arm64)
	KARCH=arm64
	TARGET=Image
	IMAGE=arch/arm64/boot/Image
	;;
*)
	echo "build.sh: unknown arch $ARCH (amd64, arm64 or all)" >&2
	exit 2
	;;
esac
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
src=$OUT/linux
build=$OUT/build-$ARCH
dist=$OUT/herd-guest-$ARCH
if [ -n "$PLAN" ]; then
	echo "$ARCH $KARCH $TARGET $dist/kernel $dist/initrd.img"
	exit 0
fi

# Per-architecture inputs win over the shared names.
pick() { eval "v=\${$1_$ARCH:-\${$1:-}}"; echo "$v"; }
HERD_GUEST=$(pick HERD_GUEST)
BUSYBOX=$(pick BUSYBOX)
CRUN=$(pick CRUN)
CROSS_COMPILE=$(pick CROSS_COMPILE)
export CROSS_COMPILE
if [ -z "$HERD_GUEST" ]; then
	HERD_GUEST=$OUT/herd-guest-$ARCH.bin
	(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -o "$HERD_GUEST" ./cmd/herd-guest)
fi
: "${BUSYBOX:?BUSYBOX or BUSYBOX_$ARCH must name a static busybox for $ARCH}"
: "${CRUN:?CRUN or CRUN_$ARCH must name a static crun for $ARCH}"
export HERD_GUEST BUSYBOX CRUN

# The initramfs: herd-guest as /init, busybox and crun, assembled by guest/initrd (gen_init_cpio's list format).
initrd() {
	mkdir -p "$dist"
	(cd "$here/.." && go run ./guest/initrd/mkinitrd "$here/initrd/initramfs.list") | gzip -9 >"$dist/initrd.img"
}
if [ -n "$INITRD" ]; then
	initrd
	echo "build.sh: $dist/initrd.img"
	exit 0
fi
if [ ! -d "$src/.git" ]; then
	git clone --depth 1 --branch "$KERNEL_REF" "$KERNEL_REPO" "$src"
fi
if [ "$(git -C "$src" rev-parse HEAD)" != "$KERNEL_COMMIT" ]; then
	echo "build.sh: $src is not $KERNEL_REF ($KERNEL_COMMIT)" >&2
	exit 1
fi

kmake() { make -C "$src" O="$build" ARCH="$KARCH" "$@"; }

kmake allnoconfig
"$src/scripts/kconfig/merge_config.sh" -m -O "$build" "$build/.config" \
	"$here/kernel/herd-guest.config" "$here/kernel/${ARCH}.config"
kmake olddefconfig
# Every symbol the fragments set must survive kconfig; a dropped one is a missing dependency, not a choice.
for frag in herd-guest.config "${ARCH}.config"; do
	grep '^CONFIG_' "$here/kernel/$frag" | while read -r want; do
		if ! grep -qx "$want" "$build/.config"; then
			echo "build.sh: $frag: $want did not survive kconfig for $ARCH" >&2
			exit 1
		fi
	done
done

kmake -j"$(nproc)" "$TARGET"
mkdir -p "$dist"
cp "$build/$IMAGE" "$dist/kernel"
cp "$build/.config" "$dist/config"
initrd
echo "build.sh: $dist"
