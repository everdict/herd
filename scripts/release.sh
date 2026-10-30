#!/bin/sh
# Builds herd's release archives: one tarball per platform with the static binaries the runbooks and dist/ install.
#
#   scripts/release.sh <out-dir>
#
# linux/amd64 and linux/arm64: herd, herd-api, herd-router, herd-egress, herdlet, all CGO_ENABLED=0 (static).
# darwin/arm64: herd-api, herd-router and herd-egress CGO_ENABLED=0; herd and herdlet drive Virtualization.framework
# through cgo, so they are built (and signed with vmm/vz/vz.entitlements) only when this script runs on a Mac.
# Set GUEST_DIR to a guest/build.sh output directory to add the guest kernel and initramfs (herd-guest-<arch>/{kernel,initrd.img}).
# VIRTIOFSD_DIR (required) is a scripts/virtiofsd/build.sh output directory: each linux archive carries its static
# virtiofsd (virtiofsd-<arch>/virtiofsd), checked against the checksum the build recorded and the version
# scripts/virtiofsd/source.env pins, because a distribution's may be older than herd needs (Ubuntu 24.04's is 1.10).
# The script ends by listing what each archive holds.
set -eu

OUT=${1:?usage: release.sh <out-dir>}
here=$(cd "$(dirname "$0")/.." && pwd)
. "$here/scripts/virtiofsd/source.env"
VIRTIOFSD_DIR=${VIRTIOFSD_DIR:?set VIRTIOFSD_DIR to a scripts/virtiofsd/build.sh all <dir> output: the linux archives carry its virtiofsd}
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
			# herdlet doctor refuses a Mac herdlet without the entitlement; fail here first.
			# PlistBuddy, not plutil -extract: plutil reads the dots in com.apple.security.virtualization as a key path.
			codesign -d --entitlements - --xml "$dir/$c" >"$dir/.$c.entitlements.plist" 2>/dev/null
			if [ "$(/usr/libexec/PlistBuddy -c 'Print :com.apple.security.virtualization' "$dir/.$c.entitlements.plist" 2>/dev/null)" != true ]; then
				rm -f "$dir/.$c.entitlements.plist"
				echo "release.sh: $dir/$c is not signed with com.apple.security.virtualization" >&2
				exit 1
			fi
			rm -f "$dir/.$c.entitlements.plist"
		else
			echo "release.sh: skipping darwin $c (Virtualization.framework needs cgo on a Mac)" >&2
		fi
	done
	if [ "$goos" = linux ]; then
		vdir="$VIRTIOFSD_DIR/virtiofsd-$goarch"
		if ! (cd "$vdir" && sha256sum -c --status virtiofsd.sha256); then
			echo "release.sh: $vdir/virtiofsd does not match its virtiofsd.sha256 (scripts/virtiofsd/build.sh $goarch)" >&2
			exit 1
		fi
		if [ "$(cat "$vdir/VERSION")" != "$VIRTIOFSD_VERSION" ]; then
			echo "release.sh: $vdir is virtiofsd $(cat "$vdir/VERSION"), scripts/virtiofsd/source.env pins $VIRTIOFSD_VERSION" >&2
			exit 1
		fi
		# ELF e_machine: x86-64 is 3e 00, AArch64 b7 00.
		want=3e00; [ "$goarch" = arm64 ] && want=b700
		if [ "$(od -An -tx1 -j18 -N2 "$vdir/virtiofsd" | tr -d ' \n')" != "$want" ]; then
			echo "release.sh: $vdir/virtiofsd is not an executable for $goarch" >&2
			exit 1
		fi
		install -m 0755 "$vdir/virtiofsd" "$dir/virtiofsd"
	fi
	if [ -n "${GUEST_DIR:-}" ] && [ -d "$GUEST_DIR/herd-guest-$goarch" ]; then
		cp "$GUEST_DIR/herd-guest-$goarch/kernel" "$GUEST_DIR/herd-guest-$goarch/initrd.img" "$dir/"
	fi
	cp -R "$here/dist" "$dir/dist"
	tar -C "$dir" -czf "$OUT/herd_${goos}_${goarch}.tar.gz" .
done
for p in linux/amd64 linux/arm64 darwin/arm64; do
	a="herd_${p%/*}_${p#*/}.tar.gz" v=
	[ "${p%/*}" = linux ] && v=" (virtiofsd $VIRTIOFSD_VERSION)"
	echo "release.sh: $a: $(tar -tzf "$OUT/$a" | sed -n 's#^\./##; /\//d; /^$/d; p' | sort | tr '\n' ' ')+ dist/$v"
done
