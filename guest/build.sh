#!/bin/sh
# Builds the herd guest kernel and initramfs for one architecture from the one shared configuration (spec R-D8).
#
#   HERD_GUEST=... BUSYBOX=... CRUN=... guest/build.sh <amd64|arm64> <out-dir>
#
# HERD_GUEST, BUSYBOX and CRUN are static binaries for the target architecture. Needs git, make, flex, bison, bc,
# libelf headers and a C toolchain for the target (set CROSS_COMPILE when building for the other architecture).
# Output: <out-dir>/herd-guest-<arch>/{kernel,initrd.img,config}.
set -eu

ARCH=${1:?usage: build.sh <amd64|arm64> <out-dir>}
OUT=${2:?usage: build.sh <amd64|arm64> <out-dir>}
here=$(cd "$(dirname "$0")" && pwd)
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
	echo "build.sh: unknown arch $ARCH (amd64 or arm64)" >&2
	exit 2
	;;
esac
: "${HERD_GUEST:?HERD_GUEST must name the herd-guest binary for $ARCH}"
: "${BUSYBOX:?BUSYBOX must name a static busybox for $ARCH}"
: "${CRUN:?CRUN must name a static crun for $ARCH}"
export HERD_GUEST BUSYBOX CRUN

mkdir -p "$OUT"
src=$OUT/linux
build=$OUT/build-$ARCH
dist=$OUT/herd-guest-$ARCH
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
"${HOSTCC:-cc}" -o "$build/gen_init_cpio" "$src/usr/gen_init_cpio.c"
mkdir -p "$dist"
cp "$build/$IMAGE" "$dist/kernel"
cp "$build/.config" "$dist/config"
"$build/gen_init_cpio" "$here/initrd/initramfs.list" | gzip -9 >"$dist/initrd.img"
echo "build.sh: $dist"
