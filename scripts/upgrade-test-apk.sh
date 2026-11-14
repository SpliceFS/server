#!/bin/sh
# The Alpine part of the upgrade test (scripts/upgrade-test.sh), run inside a throwaway
# Alpine container.
# Usage: upgrade-test-apk.sh list PACKAGE          prints the listing of one package
#        upgrade-test-apk.sh upgrade OLD NEW       installs OLD, writes markers, installs
#                                                  NEW over it and checks the result
set -eu

# shellcheck source=upgrade-lib.sh
. scripts/upgrade-lib.sh

list_package() {
    work="$(mktemp -d)"
    # An apk is three archives one after the other (signature, package data, files); tar of
    # this system reads them all, and the first two leave only names that start with a dot.
    tar -xzf "$1" -C "$work"
    list_tree "$work"
    # The package data minus what changes in every build: the version, the size, the build
    # time, the checksum of the files, the version inside "provides" and the comment lines.
    grep -v -e '^#' -e '^pkgver = ' -e '^size = ' -e '^builddate = ' -e '^datahash = ' "$work/.PKGINFO" |
        sed -e 's/^\(provides = [^=]*\)=.*/\1/' -e 's/^/meta pkginfo /'
    # The install and remove scripts; the signature is made again in every build.
    for file in "$work"/.*; do
        case "$(basename "$file")" in
            . | .. | .PKGINFO | .SIGN.*) ;;
            *) script_line "$(basename "$file")" "$file" ;;
        esac
    done
    rm -rf "$work"
}

mode="${1:-}"
case "$mode" in
    list)
        list_package "${2:?usage: upgrade-test-apk.sh list PACKAGE}"
        ;;
    upgrade)
        old="${2:?usage: upgrade-test-apk.sh upgrade OLD NEW}"
        new="${3:?usage: upgrade-test-apk.sh upgrade OLD NEW}"
        # The published files are signed with a key that is not in this container, so the
        # signature cannot be checked here; the repository tests check it.
        apk add -q --allow-untrusted "$old" || fail "apk upgrade: installing the old package failed"
        write_markers apk
        apk add -q --allow-untrusted "$new" || fail "apk upgrade: installing the new package over the old one failed"
        init="/etc/init.d/$program"
        # The files the new package lists must all be there after the upgrade.
        listing="$(mktemp)"
        list_package "$new" > "$listing"
        user="$(sed -n 's/^command_user="\([^:"]*\).*/\1/p' "$init")"
        verify_upgrade apk "$user" "$listing"
        ;;
    *)
        echo "usage: upgrade-test-apk.sh list PACKAGE | upgrade OLD NEW" >&2
        exit 2
        ;;
esac
