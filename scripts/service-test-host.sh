#!/bin/sh
# Installs the Debian package on the machine this runs on and proves the service there. The
# containers of service-test.sh run the real service manager, but they share the kernel of
# their host and start from a bare image; a CI runner is a whole machine that booted the
# normal way, with its own firewall, users and running services, so this is the closest a
# check gets to a real install. Because it changes the machine, it refuses to run anywhere
# but in CI. It removes the package again at the end.
set -eu

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
. scripts/package-lib.sh

[ "${CI:-}" = true ] || fail "this installs $name on the machine it runs on; it only runs in CI"
[ -d /run/systemd/system ] || fail "this machine does not run systemd"

port=8480
health_path=/api/v1/health
expected_user="$(sed -n 's/^User=//p' "packaging/systemd/$program.service")"
deb="dist/${name}_${debver}_$(dpkg --print-architecture).deb"
[ -f "$deb" ] || fail "$deb does not exist; run make package first"

# Runs the command once a second until it succeeds or the given number of seconds is over.
wait_for() {
    limit="$1"
    shift
    waited=0
    until "$@"; do
        waited=$((waited + 1))
        [ "$waited" -lt "$limit" ] || return 1
        sleep 1
    done
}

answers() {
    code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 3 \
        "https://127.0.0.1:$port$health_path" || true)"
    [ "$code" = 200 ]
}

active() {
    systemctl is-active --quiet "$program.service"
}

report() {
    systemctl status "$program.service" --no-pager || true
    sudo journalctl -u "$program.service" -n 30 --no-pager || true
}

# The package must go even when a check fails, or a rerun on the same machine starts dirty.
remove() {
    sudo apt-get purge -y -qq "$name" >/dev/null 2>&1 || true
}
trap remove EXIT

sudo apt-get install -y -qq "./$deb" >/dev/null
# Nobody starts it: installing the package must be enough.
systemctl is-enabled --quiet "$program.service" || { report; fail "the service is not enabled after the install"; }
wait_for 20 active || { report; fail "the service is not running after the install"; }
pid="$(systemctl show -p MainPID --value "$program.service")"
owner="$(ps -o user= -p "$pid" | tr -d ' ')"
[ "$owner" = "$expected_user" ] || fail "the service runs as '$owner', not as $expected_user"
wait_for 10 answers || { report; fail "the server does not answer on $health_path"; }

sudo systemctl restart "$program.service"
wait_for 20 active || { report; fail "the service is not running after a restart"; }
wait_for 10 answers || { report; fail "the server does not answer after a restart"; }

sudo apt-get purge -y -qq "$name" >/dev/null
trap - EXIT
if active; then fail "the service is still running after the package was removed"; fi
if answers; then fail "the port still answers after the package was removed"; fi

. /etc/os-release
echo "service test passed on this machine ($PRETTY_NAME, $(uname -m)): $(basename "$deb")"
