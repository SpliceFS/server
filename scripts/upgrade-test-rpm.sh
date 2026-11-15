#!/bin/sh
# The rpm part of the upgrade test (scripts/upgrade-test.sh), run inside a throwaway
# container of an rpm based system.
# Usage: upgrade-test-rpm.sh list PACKAGE          prints the listing of one package
#        upgrade-test-rpm.sh upgrade OLD NEW       installs OLD, writes markers, upgrades
#                                                  to NEW and checks the result
set -eu

# shellcheck source=upgrade-lib.sh
. scripts/upgrade-lib.sh

list_package() {
    # rpm reads the package itself, so nothing is extracted. A folder has no content sum and
    # only a link has a target; "-" stands for each of them so every line has the same fields.
    rpm -qp --qf '[%{FILEMODES:perms}|%{FILEUSERNAME}:%{FILEGROUPNAME}|%{FILEDIGESTS}|%{FILELINKTOS}|%{FILENAMES}\n]' "$1" |
        awk -F'|' '{
            kind = "file"
            if (substr($1, 1, 1) == "d") kind = "dir"
            if (substr($1, 1, 1) == "l") kind = "link"
            detail = $3
            if (kind == "link") detail = $4
            if (detail == "") detail = "-"
            print kind, $1, $2, detail, $5
        }'
    # The package's own declaration of what it needs. The rpmlib lines name what the rpm
    # program that built the package could do, not what the package needs.
    rpm -qp --qf '[%{REQUIRENAME} %{REQUIREFLAGS:deptype} %{REQUIREFLAGS:depflags} %{REQUIREVERSION}\n]' "$1" |
        grep -v '^rpmlib(' | sed 's/^/meta requires /'
    rpm -qp --qf '[%{PROVIDENAME}\n]' "$1" | sed 's/^/meta provides /'
    # The name, CPU type, license and texts. The version and release change in every build.
    rpm -qp --qf 'meta name %{NAME}\nmeta arch %{ARCH}\nmeta license %{LICENSE}\nmeta url %{URL}\nmeta summary %{SUMMARY}\n' "$1"
    # A package without such a script prints "(none)", which is the same in every version.
    for tag in PREIN POSTIN PREUN POSTUN PRETRANS POSTTRANS; do
        sum="$(rpm -qp --qf "%{$tag}" "$1" | sha256sum)"
        printf 'script %s %s\n' "$tag" "${sum%% *}"
    done
}

mode="${1:-}"
case "$mode" in
    list)
        list_package "${2:?usage: upgrade-test-rpm.sh list PACKAGE}"
        ;;
    upgrade)
        old="${2:?usage: upgrade-test-rpm.sh upgrade OLD NEW}"
        new="${3:?usage: upgrade-test-rpm.sh upgrade OLD NEW}"
        rpm -i "$old" >/dev/null 2>&1 || fail "rpm upgrade: installing the old package failed"
        write_markers rpm
        rpm -U "$new" >/dev/null 2>&1 || fail "rpm upgrade: upgrading to the new package failed"
        unit="/usr/lib/systemd/system/$program.service"
        # The files the new package lists must all be there after the upgrade.
        listing="$(mktemp)"
        list_package "$new" > "$listing"
        user="$(sed -n 's/^User=//p' "$unit")"
        verify_upgrade rpm "$user" "$listing"
        ;;
    *)
        echo "usage: upgrade-test-rpm.sh list PACKAGE | upgrade OLD NEW" >&2
        exit 2
        ;;
esac
