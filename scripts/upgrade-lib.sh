# shellcheck shell=sh
# Sourced by scripts/upgrade-test-deb.sh, -rpm.sh and -apk.sh, which run inside a container
# of a test system. The scripts that source this file read these variables; this file reads
# none of them.
# shellcheck disable=SC2034
#
# A package listing is what scripts/upgrade-diff.sh compares. One line per fact, fields
# separated by one space, no field holds a space except the last one of a "meta" line:
#   file MODE OWNER:GROUP SHA256 /path      a regular file and its content
#   dir  MODE OWNER:GROUP - /path           a folder
#   link MODE OWNER:GROUP TARGET /path      a symbolic link
#   script NAME SHA256                      an install or remove script of the package
#   meta TEXT                               a declared dependency, a configuration file or
#                                           other package data, with what changes in every
#                                           version (version, size, build time) left out
# Both formats of one version must be written by the same code, so a difference in a listing
# is a difference in the package and never in the way it was read.
: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

datadir="/var/lib/splicefs/${name#splicefs-}"
# The package ships no configuration file, only empty folders for the administrator's files
# (packaging/config-folders), so the file an administrator would write there is the one the
# upgrade must leave alone. The package tests use the same name.
keptdir="$confdir/$(printf '%s\n' "$confsubdirs" | head -n 1)"
kept="$keptdir/local.yaml"
marker="# upgrade-test marker: an upgrade must keep this line"
datafile="$datadir/upgrade-test-marker"

# Prints the listing lines of every entry under folder $1, which holds a package's files
# extracted as root. A top level name that starts with a dot is package data (Alpine keeps
# its scripts and .PKGINFO there) and is not a file of the package. The names in a package
# hold no spaces or newlines.
list_tree() {
    find "$1" -mindepth 1 ! -path "$1/.*" | LC_ALL=C sort | while read -r path; do
        attrs="$(stat -c '%a %U:%G' "$path")"
        if [ -L "$path" ]; then
            kind='link'
            detail="$(readlink "$path")"
        elif [ -d "$path" ]; then
            kind='dir'
            detail=-
        else
            kind='file'
            detail="$(sha256sum "$path")"
            detail="${detail%% *}"
        fi
        printf '%s %s %s /%s\n' "$kind" "$attrs" "$detail" "${path#"$1"/}"
    done
}

# Prints the listing line of the script in file $2, called $1 in the package.
script_line() {
    sum="$(sha256sum "$2")"
    printf 'script %s %s\n' "$1" "${sum%% *}"
}

# The checks after the new package was installed over the old one, which also had its
# configuration file and data written by the administrator. $1 is the package format, only
# for the messages, $2 the user the service runs as, $3 the listing of the new package.
verify_upgrade() {
    "$program" --version | grep -qF "$VERSION" ||
        fail "$1 upgrade: $program --version does not report $VERSION"
    grep -qxF "$marker" "$kept" || fail "$1 upgrade: the marker line is gone from $kept"
    [ -f "$datafile" ] || fail "$1 upgrade: the marker file $datafile is gone"
    if [ -n "$2" ]; then
        grep -q "^$2:" /etc/passwd || fail "$1 upgrade: the user $2 does not exist"
    fi
    # The listing has the path last on every file, folder and link line.
    awk '$1 == "file" || $1 == "dir" || $1 == "link" { print $NF }' "$3" | while read -r path; do
        [ -e "$path" ] || [ -L "$path" ] || fail "$1 upgrade: $path is listed in the package and missing"
    done
}

# Writes the marker line and the marker file, the way an administrator and the running
# server would have, before the upgrade.
write_markers() {
    [ -d "$keptdir" ] || fail "$1 upgrade: $keptdir is missing after the install"
    printf '%s\n' "$marker" >> "$kept"
    mkdir -p "$datadir"
    printf 'written before the upgrade\n' > "$datafile"
}
