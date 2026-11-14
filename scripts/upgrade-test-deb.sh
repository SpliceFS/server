#!/bin/sh
# The Debian part of the upgrade test (scripts/upgrade-test.sh), run inside a throwaway
# Debian or Ubuntu container.
# Usage: upgrade-test-deb.sh list PACKAGE          prints the listing of one package
#        upgrade-test-deb.sh upgrade OLD NEW       installs OLD, writes markers, installs
#                                                  NEW over it and checks the result
set -eu

# shellcheck source=upgrade-lib.sh
. scripts/upgrade-lib.sh

list_package() {
    work="$(mktemp -d)"
    dpkg-deb -x "$1" "$work/data"
    dpkg-deb -e "$1" "$work/control"
    list_tree "$work/data"
    # The version and the size change with every build and say nothing about the packaging.
    grep -v -e '^Version:' -e '^Installed-Size:' "$work/control/control" | sed 's/^/meta control /'
    for file in "$work/control"/*; do
        case "$(basename "$file")" in
            # The list of file sums repeats what the file lines already say.
            control | md5sums) ;;
            conffiles) sed 's/^/meta conffile /' "$file" ;;
            *) script_line "$(basename "$file")" "$file" ;;
        esac
    done
    rm -rf "$work"
}

mode="${1:-}"
case "$mode" in
    list)
        list_package "${2:?usage: upgrade-test-deb.sh list PACKAGE}"
        ;;
    upgrade)
        old="${2:?usage: upgrade-test-deb.sh upgrade OLD NEW}"
        new="${3:?usage: upgrade-test-deb.sh upgrade OLD NEW}"
        # A user installs a downloaded file with apt, which also fetches what it needs.
        apt-get install -y -qq "$old" >/dev/null || fail "deb upgrade: installing the old package failed"
        write_markers deb
        apt-get install -y -qq "$new" >/dev/null || fail "deb upgrade: installing the new package over the old one failed"
        unit="/usr/lib/systemd/system/$program.service"
        # The files the new package lists must all be there after the upgrade.
        listing="$(mktemp)"
        list_package "$new" > "$listing"
        user="$(sed -n 's/^User=//p' "$unit")"
        verify_upgrade deb "$user" "$listing"
        ;;
    *)
        echo "usage: upgrade-test-deb.sh list PACKAGE | upgrade OLD NEW" >&2
        exit 2
        ;;
esac
