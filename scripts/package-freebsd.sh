#!/bin/sh
# Builds the FreeBSD packages and proves them, on FreeBSD itself: for every FreeBSD release
# in packaging/freebsd/images it starts that release as a virtual machine, where pkg(8)
# builds the package for amd64 and arm64, installs the one for the machine and checks that
# the service runs (scripts/freebsd-job.sh). FreeBSD cannot run in a container, so the
# machine runs under QEMU inside the Go container; nothing but Docker, curl and xz is needed
# here. It runs on the host (make package-freebsd) after package-binaries.sh.
#
# Environment: VERSION and PACKAGE_DATE as for every package; FREEBSD limits the run to
# one release (for example 15.1), empty means every release in the list; LINUX_IMAGE is
# the container image QEMU is installed into; VM_TIMEOUT is the most seconds one machine
# may run.
set -eu

out="${1:?usage: package-freebsd.sh OUTPUT_DIR}"
. scripts/package-lib.sh
image="${LINUX_IMAGE:?LINUX_IMAGE is not set}"
out="$(cd "$out" && pwd)"

# The machine has the CPU type of this computer: its own type runs fastest, and with KVM
# (Linux on amd64) at full speed.
case "$(uname -m)" in
    x86_64 | amd64) machine=amd64 ;;
    arm64 | aarch64) machine=arm64 ;;
    *) fail "no FreeBSD image for $(uname -m)" ;;
esac
for arch in amd64 arm64; do
    [ -f "$out/stage/freebsd-$arch/$program" ] ||
        fail "$out/stage/freebsd-$arch/$program does not exist; run package-binaries.sh first"
done

checksum() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1
}

cache="$out/cache/freebsd"
mkdir -p "$cache"
kvm=""
if [ "$machine" = amd64 ] && [ -e /dev/kvm ]; then kvm="--device /dev/kvm"; fi

ran=0
while read -r release image_machine sum url; do
    case "$release" in "" | "#"*) continue ;; esac
    [ "$image_machine" = "$machine" ] || continue
    [ -z "${FREEBSD:-}" ] || [ "$FREEBSD" = "$release" ] || continue
    ran=$((ran + 1))

    packed="$cache/$(basename "$url")"
    disk="${packed%.xz}"
    if [ ! -f "$disk" ]; then
        echo "fetching FreeBSD $release ($machine)"
        curl -fsS -o "$packed.part" "$url"
        [ "$(checksum "$packed.part")" = "$sum" ] || fail "$url does not have the checksum in packaging/freebsd/images"
        mv "$packed.part" "$packed"
        xz -d "$packed"
    fi

    # Everything the machine reads sits in one folder, which it sees as a small disk named
    # cidata. FreeBSD runs the file user-data from such a disk on its first boot.
    job="$out/freebsd-job/$release"
    rm -rf "$job"
    mkdir -p "$job"
    for arch in amd64 arm64; do
        cp "$out/stage/freebsd-$arch/$program" "$job/$program-$arch"
    done
    cp "packaging/freebsd/$program.rc" packaging/freebsd/manifest.in packaging/freebsd/plist \
        scripts/freebsd-job.sh "$job/"
    sed -e "s|@NAME@|$name|" -e "s|@PROGRAM@|$program|" -e "s|@VERSION@|$pkgver|" \
        -e "s|@PROGRAM_VERSION@|$VERSION|" packaging/freebsd/user-data.in > "$job/user-data"
    printf 'instance-id: %s-%s\nlocal-hostname: build\n' "$name" "$release" > "$job/meta-data"

    echo "FreeBSD $release ($machine): building and testing $name $pkgver"
    # kvm is one option or empty, so it is left unquoted on purpose.
    # shellcheck disable=SC2086
    docker run --rm $kvm -e VM_TIMEOUT="${VM_TIMEOUT:-900}" -v "$(pwd)":/src:ro -v "$out":/out "$image" \
        sh /src/scripts/freebsd-vm.sh "$machine" "/out/cache/freebsd/$(basename "$disk")" \
        "/out/freebsd-job/$release" "/out/freebsd" || fail "FreeBSD $release ($machine) failed"
    echo "ok   FreeBSD $release ($machine)"
done < packaging/freebsd/images
[ "$ran" -gt 0 ] || fail "no image in packaging/freebsd/images for ${FREEBSD:-any release} on $machine"
ls "$out"/freebsd/*/*.pkg
