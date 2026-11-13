#!/bin/sh
# Runs inside the FreeBSD build machine as root, started by the first-boot script. It builds
# the packages with FreeBSD's own tools, installs the one for this machine and proves that the
# service runs.
#
#   freebsd-job.sh <input folder> <output folder> <package name> <program> <package version> <program version>
set -eu
in="$1"
out="$2"
name="$3"
program="$4"
version="$5"
program_version="$6"
mkdir -p "$out"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

major="$(freebsd-version -u | cut -d. -f1)"
machine="$(uname -m)"

# The base system ships only a stub that fetches the real pkg on first use.
pkg -N >/dev/null 2>&1 || env ASSUME_ALWAYS_YES=yes pkg bootstrap -y >/dev/null

# One package per CPU type. goarch is how the build names the binary, abiarch how pkg names
# the CPU type in an ABI string.
build() {
    goarch="$1"
    abiarch="$2"
    stage="/tmp/stage-$goarch"
    rm -rf "$stage"
    install -d "$stage/usr/local/bin" "$stage/usr/local/etc/rc.d" \
        "$stage/usr/local/etc/splicefs/server/conf.d" "$stage/usr/local/etc/splicefs/server/shares.d"
    install -m 0755 "$in/$program-$goarch" "$stage/usr/local/bin/$program"
    install -m 0755 "$in/$program.rc" "$stage/usr/local/etc/rc.d/$program"
    sed -e "s|@NAME@|$name|" -e "s|@PROGRAM@|$program|g" -e "s|@VERSION@|$version|" -e "s|@ABI@|FreeBSD:$major:$abiarch|" "$in/manifest.in" > "/tmp/manifest-$goarch"
    mkdir -p "$out/FreeBSD-$major-$abiarch"
    pkg -o ABI="FreeBSD:$major:$abiarch" -o OSVERSION="$(uname -K)" create -M "/tmp/manifest-$goarch" -p "$in/plist" \
        -r "$stage" -o "$out/FreeBSD-$major-$abiarch"
}
build amd64 amd64
build arm64 aarch64

case "$machine" in
    amd64) mine="$out/FreeBSD-$major-amd64" ;;
    arm64) mine="$out/FreeBSD-$major-aarch64" ;;
    *) fail "unknown machine $machine" ;;
esac
package="$mine/$name-$version.pkg"
[ -f "$package" ] || fail "pkg create did not write $package"
pkg info -F "$package" > "$out/info-$machine.txt"

answers() {
    fetch --no-verify-peer -qo /dev/null https://127.0.0.1:8480/api/v1/health 2>/dev/null
}
# Waits until the command given succeeds, at most $1 seconds.
wait_for() {
    limit="$1"
    shift
    n=0
    until "$@" >/dev/null 2>&1; do
        n=$((n + 1))
        [ "$n" -lt "$limit" ] || return 1
        sleep 1
    done
}
# What a person would look at first when the service misbehaves.
show() {
    service "$program" status || true
    ls -la "/var/run/$program" || true
    head "/var/run/$program"/*.pid || true
    ps axww -o pid,jid,user,command | grep -e "$program" -e '^ *PID' | grep -v grep || true
    sh -x "/usr/local/etc/rc.d/$program" status 2>&1 | tail -n 25 || true
    tail -n 30 "/var/log/$program.log" 2>/dev/null || true
}

pkg add "$package"
[ -x "/usr/local/bin/$program" ] || fail "the program is not installed"
"/usr/local/bin/$program" --version | grep -qF "$program_version" || fail "$program --version does not report $program_version"
# pkg ends every process an install script leaves behind, so on FreeBSD a package cannot
# start its service. What the package can and must do: the service is enabled without any
# setting, so it starts at boot, and one command starts it now.
service "$program" enabled || fail "the service is not enabled after the install"
service "$program" start || { show; fail "the service did not start"; }
wait_for 20 service "$program" status || { show; fail "the service is not running after its start"; }
wait_for 20 answers || { show; fail "the server does not answer after its start"; }
owner="$(ps -o user= -p "$(cat "/var/run/$program/$program.pid")")"
[ "$owner" = splicefs ] || fail "the server runs as '$owner', not as splicefs"
service "$program" reload || fail "reload failed"
wait_for 10 answers || { show; fail "the server does not answer after a reload"; }
service "$program" restart >/dev/null || fail "restart failed"
wait_for 20 answers || { show; fail "the server does not answer after a restart"; }
echo "installed, enabled, started with one command, running as splicefs, answering, reload and restart work"

# Removing the package stops the service and leaves nothing running.
pkg delete -y "$name" >/dev/null
if answers; then fail "the server still answers after the package was removed"; fi
[ ! -e "/usr/local/bin/$program" ] || fail "remove left the program behind"
echo "removed: the service stopped"
