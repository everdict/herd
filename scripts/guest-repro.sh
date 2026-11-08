#!/bin/sh
# Checks that the herd guest builds reproducibly (DEFAUL-1626): it builds the guest twice, in two fresh clones of
# this repository at two fresh paths with two empty Go build caches, and fails when the artifacts differ. herd-api
# rebuilds every golden whose guest no worker runs (deploy.GuestIdentity: the digest of the kernel and initrd.img),
# so a guest that is not the same bytes from the same source rebuilds every golden at every release.
#
#   scripts/guest-repro.sh [--kernel] [<rev-a> [<rev-b>]]
#
# rev-a defaults to HEAD and rev-b to rev-a; with two revisions that differ in nothing the guest is built from (a
# release of herdlet only), the artifacts must still be the same. The clones are of committed revisions: commit
# first. Without --kernel it builds herd-guest and initrd.img for amd64 and arm64 (guest/build.sh --initrd), which
# needs only git, go and gzip; with --kernel it runs guest/build.sh in full and also compares each kernel and the
# guest identity herd computes from both (needs network for the kernel clone, a C toolchain and the guest/build.sh
# prerequisites). busybox and crun are BUSYBOX and CRUN (or BUSYBOX_<arch>, CRUN_<arch>) when set, else fixed
# stand-in files (a stand-in initrd boots nothing; it holds the assembly to account); each side gets its own copy
# with a different modification time, as two downloads would have.
set -eu

KERNEL= MODE=--initrd
if [ "${1:-}" = --kernel ]; then
	KERNEL=1 MODE=
	shift
fi
here=$(cd "$(dirname "$0")/.." && pwd)
REV_A=$(git -C "$here" rev-parse "${1:-HEAD}")
REV_B=$(git -C "$here" rev-parse "${2:-$REV_A}")
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

side() { # dir rev stamp: clone rev into <dir>/src (a different path per side) and build its guest
	d=$1
	mkdir -p "$d/in"
	git clone -q "$here" "$d/src"
	git -C "$d/src" checkout -q --detach "$2"
	for a in amd64 arm64; do
		for b in busybox crun; do
			B=$(echo "$b" | tr a-z A-Z)
			eval "given=\${${B}_$a:-\${$B:-}}"
			if [ -n "$given" ]; then
				cp "$given" "$d/in/$b-$a"
			else
				printf '#!/bin/false\n# a stand-in for a static %s for %s\n' "$b" "$a" >"$d/in/$b-$a"
			fi
			touch -d "@$3" "$d/in/$b-$a"
			eval "${B}_$a=\$d/in/\$b-\$a; export ${B}_$a"
		done
	done
	GOCACHE=$d/gocache sh "$d/src/guest/build.sh" $MODE all "$d/out" >"$d/build.log" 2>&1 || {
		echo "guest-repro: the build of $2 in $d failed:" >&2
		cat "$d/build.log" >&2
		exit 1
	}
}

side "$tmp/a" "$REV_A" 1700000000
side "$tmp/b/c" "$REV_B" 1760000000

files="initrd.img"
[ -n "$KERNEL" ] && files="kernel initrd.img"
bad=0
for a in amd64 arm64; do
	for f in $files herd-guest-$a.bin; do
		case $f in
		*.bin) pa=$tmp/a/out/$f pb=$tmp/b/c/out/$f ;;
		*) pa=$tmp/a/out/herd-guest-$a/$f pb=$tmp/b/c/out/herd-guest-$a/$f ;;
		esac
		da=$(sha256sum "$pa" | cut -d' ' -f1)
		db=$(sha256sum "$pb" | cut -d' ' -f1)
		label=$a/$f
		[ "${f%.bin}" != "$f" ] && label="$a/herd-guest (/init)"
		if [ "$da" = "$db" ]; then
			echo "guest-repro: $label sha256:$da on both"
		else
			echo "guest-repro: $label differs: sha256:$da and sha256:$db" >&2
			bad=1
		fi
	done
	if [ -n "$KERNEL" ]; then
		id() { printf 'kernel sha256:%s\ninitrd sha256:%s\n' "$(sha256sum "$1/kernel" | cut -d' ' -f1)" "$(sha256sum "$1/initrd.img" | cut -d' ' -f1)" | sha256sum | cut -d' ' -f1; }
		echo "guest-repro: $a guest identity sha256:$(id "$tmp/a/out/herd-guest-$a") and sha256:$(id "$tmp/b/c/out/herd-guest-$a")"
	fi
done
if [ -z "$KERNEL" ]; then
	echo "guest-repro: not compared: the kernels (run with --kernel where they can be built)"
fi
if [ "$bad" != 0 ]; then
	echo "guest-repro: FAIL: the guest is not reproducible from $REV_A and $REV_B" >&2
	exit 1
fi
echo "guest-repro: ok: $REV_A and $REV_B build the same guest"
