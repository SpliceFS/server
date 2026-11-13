#!/bin/sh
# Runs one FreeBSD virtual machine to completion: it boots the image, the machine runs the
# first-boot script of the job folder, writes its results to a scratch disk and powers off.
# It runs inside the Go container (scripts/package-freebsd.sh), which gets QEMU installed
# for the length of this one run.
#
#   freebsd-vm.sh <amd64|arm64> <disk image> <job folder> <result folder>
set -eu

machine="$1"
disk="$2"
job="$3"
results="$4"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
case "$machine" in
    amd64) apt-get install -y -qq --no-install-recommends qemu-system-x86 qemu-utils >/dev/null 2>&1 ;;
    arm64) apt-get install -y -qq --no-install-recommends qemu-system-arm qemu-efi-aarch64 qemu-utils >/dev/null 2>&1 ;;
esac

work="$(mktemp -d)"
# The image is never written to: the machine works on a copy-on-write layer over it.
qemu-img create -q -f qcow2 -F qcow2 -b "$disk" "$work/disk.qcow2"
# The machine writes a tar archive straight onto this disk, so no filesystem has to be
# shared between two operating systems.
truncate -s 256M "$work/out.img"

# Hardware virtualization when the computer offers it for this CPU type, emulation otherwise.
accel="tcg,thread=multi"
cpu=max
if [ -w /dev/kvm ] && [ "$machine" = amd64 ]; then
    accel=kvm
    cpu=host
fi
case "$machine" in
    amd64) set -- qemu-system-x86_64 -machine q35 ;;
    arm64) set -- qemu-system-aarch64 -machine virt -bios /usr/share/qemu-efi-aarch64/QEMU_EFI.fd ;;
esac

mkdir -p "$results"
console="$results/console-$(basename "$job")-$machine.log"
# The job folder is attached through a throwaway layer (snapshot=on) because FreeBSD
# mounts it read-write and the folder itself must stay untouched. romfile= spares the
# network card a boot ROM that the container does not have and nothing needs.
status=0
timeout "${VM_TIMEOUT:-900}" "$@" -cpu "$cpu" -accel "$accel" -smp 4 -m 2048 \
    -drive "if=virtio,file=$work/disk.qcow2,format=qcow2" \
    -drive "if=virtio,driver=vvfat,dir=$job,label=cidata,snapshot=on" \
    -drive "if=virtio,file=$work/out.img,format=raw" \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0,romfile= \
    -display none -serial "file:$console" -no-reboot || status=$?

got="$work/got"
mkdir -p "$got"
tar xf "$work/out.img" -C "$got" 2>/dev/null || true
if [ -f "$got/log.txt" ]; then cat "$got/log.txt"; fi
if [ "$status" -ne 0 ] || [ ! -f "$got/ok" ]; then
    echo "the FreeBSD machine did not finish its job (qemu status $status); its console:" >&2
    tail -n 40 "$console" >&2 || true
    exit 1
fi
rm -f "$got/ok" "$got/log.txt"
cp -R "$got"/. "$results"/
