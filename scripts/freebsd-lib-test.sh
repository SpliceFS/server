#!/bin/sh
# Host test of freebsd-lib.sh: no download, no machine. freebsd_each_release must call its step
# for every release in the list, whatever names the step uses for its own variables (shell
# variables are global, so a step's loop variable must not replace the lib's).
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() {
    echo "FAIL: $1" >&2
    exit 1
}
timed() {
    shift
    "$@"
}

# shellcheck source=freebsd-lib.sh
. "$root/scripts/freebsd-lib.sh"
# The lib sets machine from this computer; the list below names that machine, and its disks
# are already in the cache, so nothing is downloaded or unpacked.
out="$work/out"
mkdir -p "$work/packaging/freebsd" "$out/cache/freebsd"
for release in 14.5 15.1; do
    printf '%s %s 0 https://example.invalid/%s.raw.xz\n' "$release" "$machine" "$release" >>"$work/packaging/freebsd/images"
    : >"$out/cache/freebsd/$release.raw"
done

calls=""
# A step that loops with a variable named like the lib's own: each.
step() {
    for each in first second; do : "$each"; done
    calls="$calls $1"
}
# A step the lib cannot find ends this script with "not found" (set -e).
cd "$work"
freebsd_each_release step
[ "$calls" = " 14.5 15.1" ] || fail "the steps ran for [$calls], want [ 14.5 15.1]"
echo "freebsd-lib: every release ran its step"

# freebsd-with-kvm.sh runs its command only when it can write to the hardware virtualization
# device. The device here is a file of the test's own, because the real one exists on few
# computers.
expect() {
    want_status="$1"
    want_text="$2"
    shift 2
    status=0
    "$@" >"$work/output" 2>&1 || status=$?
    [ "$status" -eq "$want_status" ] ||
        { cat "$work/output" >&2; fail "[$*] ended with status $status, want $want_status"; }
    grep -qF -- "$want_text" "$work/output" ||
        { cat "$work/output" >&2; fail "[$*] did not say [$want_text]"; }
}
with_kvm="$root/scripts/freebsd-with-kvm.sh"
: >"$work/a-file"
expect 2 "usage" sh "$with_kvm" /dev/null
expect 1 "FAIL: $work/no-kvm" sh "$with_kvm" "$work/no-kvm" echo ran
expect 1 "FAIL: $work/a-file" sh "$with_kvm" "$work/a-file" echo ran
! grep -q '^ran$' "$work/output" || fail "the command ran without the device"
expect 0 "ran with --one -two" sh "$with_kvm" /dev/null echo ran with --one -two
echo "freebsd-with-kvm: the machine is not started without the device"

# freebsd-package-check.sh proves that a package is for the CPU type it is named for: its
# manifest names that ABI and its program is a program of that CPU type. The packages here are
# archives of the same shape as the real ones (the manifest, then the files), and the programs
# are no more than an ELF header, which is all the check reads.
check_package="$root/scripts/freebsd-package-check.sh"
# make_package <file> <abi> <machine type of the program> [manifest layout]
# The machine type is the two bytes after the file type in an ELF header, as octal escapes.
make_package() {
    stage="$work/stage"
    rm -rf "$stage"
    mkdir -p "$stage/usr/local/bin"
    if [ "${4:-}" = spaced ]; then
        printf '{\n  "name": "splicefs-server",\n  "abi": "%s",\n}\n' "$2" >"$stage/+MANIFEST"
    else
        printf '{"name":"splicefs-server","abi":"%s"}' "$2" >"$stage/+MANIFEST"
    fi
    printf '\177ELF\002\001\001\011\000\000\000\000\000\000\000\000\002\000%b' "$3" \
        >"$stage/usr/local/bin/splicefsd"
    tar -czf "$1" -C "$stage" +MANIFEST usr
}
amd64_type='\0076\0000'
arm64_type='\0267\0000'
make_package "$work/amd64.pkg" FreeBSD:15:amd64 "$amd64_type"
make_package "$work/arm64.pkg" FreeBSD:15:aarch64 "$arm64_type"
make_package "$work/spaced.pkg" FreeBSD:15:aarch64 "$arm64_type" spaced
make_package "$work/arm64-with-amd64-program.pkg" FreeBSD:15:aarch64 "$amd64_type"
make_package "$work/amd64-manifest.pkg" FreeBSD:15:amd64 "$arm64_type"
expect 0 "FreeBSD:15:amd64" sh "$check_package" "$work/amd64.pkg" 15 amd64 splicefsd
expect 0 "FreeBSD:15:aarch64" sh "$check_package" "$work/arm64.pkg" 15 aarch64 splicefsd
expect 0 "FreeBSD:15:aarch64" sh "$check_package" "$work/spaced.pkg" 15 aarch64 splicefsd
# The manifest and the program must each be right on their own.
expect 1 "does not say FreeBSD:15:aarch64" sh "$check_package" "$work/amd64.pkg" 15 aarch64 splicefsd
expect 1 "does not say FreeBSD:14:aarch64" sh "$check_package" "$work/arm64.pkg" 14 aarch64 splicefsd
expect 1 "machine type 3e00, not b700" sh "$check_package" "$work/arm64-with-amd64-program.pkg" 15 aarch64 splicefsd
expect 1 "machine type b700, not 3e00" sh "$check_package" "$work/amd64-manifest.pkg" 15 amd64 splicefsd
expect 1 "holds no usr/local/bin/other" sh "$check_package" "$work/arm64.pkg" 15 aarch64 other
expect 1 "cannot unpack $work/missing.pkg" sh "$check_package" "$work/missing.pkg" 15 aarch64 splicefsd
expect 2 "usage" sh "$check_package" "$work/arm64.pkg" 15 sparc64 splicefsd
echo "freebsd-package-check: a package is for the CPU type it says"
