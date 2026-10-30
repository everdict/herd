#!/bin/sh
# Builds the static virtiofsd herd's Linux release archives carry, from the upstream source pinned in source.env, for
# one architecture or both (`all`), the way upstream's CI builds its static binary (musl, static libseccomp and
# libcap-ng). scripts/release.sh packs it per platform (VIRTIOFSD_DIR); the runbooks install it at
# /usr/local/libexec/herd/virtiofsd, herdlet's default -virtiofsd.
#
#   scripts/virtiofsd/build.sh <amd64|arm64|all> <out-dir>
#
# Off Alpine it runs itself in $VIRTIOFSD_IMAGE with docker (DOCKER=podman works too) for linux/<arch>; the other
# architecture needs binfmt emulation on the build host (e.g. docker run --privileged --rm tonistiigi/binfmt --install
# arm64), or run it on a host of that architecture.
# Output: <out-dir>/virtiofsd-<arch>/{virtiofsd,virtiofsd.sha256,VERSION}.
set -eu

ARCH=${1:?usage: build.sh <amd64|arm64|all> <out-dir>}
OUT=${2:?usage: build.sh <amd64|arm64|all> <out-dir>}
here=$(cd "$(dirname "$0")" && pwd)
. "$here/source.env"

if [ "$ARCH" = all ]; then
	for a in amd64 arm64; do
		sh "$0" "$a" "$OUT"
	done
	exit 0
fi
case "$ARCH" in
amd64) TRIPLE=x86_64-unknown-linux-musl MACHINE=x86_64 ;;
arm64) TRIPLE=aarch64-unknown-linux-musl MACHINE=aarch64 ;;
*)
	echo "build.sh: unknown arch $ARCH (amd64, arm64 or all)" >&2
	exit 2
	;;
esac
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

if [ ! -f /etc/alpine-release ]; then
	exec ${DOCKER:-docker} run --rm --platform "linux/$ARCH" -v "$here:/build:ro" -v "$OUT:/out" "$VIRTIOFSD_IMAGE" \
		sh /build/build.sh "$ARCH" /out
fi
if [ "$(uname -m)" != "$MACHINE" ]; then
	echo "build.sh: this container is $(uname -m), not $MACHINE (docker --platform linux/$ARCH)" >&2
	exit 1
fi

apk add --no-cache git libcap-ng-static libseccomp-static musl-dev binutils
src=$(mktemp -d)/virtiofsd
git clone --depth 1 --branch "$VIRTIOFSD_REF" "$VIRTIOFSD_REPO" "$src"
if [ "$(git -C "$src" rev-parse HEAD)" != "$VIRTIOFSD_COMMIT" ]; then
	echo "build.sh: $VIRTIOFSD_REPO $VIRTIOFSD_REF is $(git -C "$src" rev-parse HEAD), not $VIRTIOFSD_COMMIT" >&2
	exit 1
fi
# Upstream's publish job, with --locked: every dependency must match Cargo.lock's checksums.
(cd "$src" && RUSTFLAGS='-C target-feature=+crt-static -C link-self-contained=yes' \
	LIBSECCOMP_LINK_TYPE=static LIBSECCOMP_LIB_PATH=/usr/lib LIBCAPNG_LINK_TYPE=static LIBCAPNG_LIB_PATH=/usr/lib \
	cargo build --locked --release --target "$TRIPLE")
bin=$src/target/$TRIPLE/release/virtiofsd

# What herdlet doctor checks on the host, checked here first: static, the pinned version, herd's options.
if readelf -l "$bin" | grep -q INTERP; then
	echo "build.sh: $bin is dynamically linked" >&2
	exit 1
fi
if ! "$bin" --version | grep -qw "$VIRTIOFSD_VERSION"; then
	echo "build.sh: $bin --version is $("$bin" --version), not $VIRTIOFSD_VERSION" >&2
	exit 1
fi
for opt in --socket-path --shared-dir --cache --migration-mode --readonly; do
	if ! "$bin" --help | grep -q -- "$opt"; then
		echo "build.sh: $bin does not accept $opt" >&2
		exit 1
	fi
done

dist=$OUT/virtiofsd-$ARCH
mkdir -p "$dist"
install -m 0755 "$bin" "$dist/virtiofsd"
echo "$VIRTIOFSD_VERSION" >"$dist/VERSION"
(cd "$dist" && sha256sum virtiofsd >virtiofsd.sha256)
echo "build.sh: $dist (virtiofsd $VIRTIOFSD_VERSION, $(cut -d' ' -f1 "$dist/virtiofsd.sha256"))"
