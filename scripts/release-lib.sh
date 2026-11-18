# shellcheck shell=sh
# Sourced by the scripts that make the release files (build-all.sh, archive-programs.sh and
# release-assets.sh), which read what it sets. Says what is released and what its files are
# called. Run from the repository top.
: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"

# The name of the release files follows the repository, whose module path ends in its name.
name="splicefs-$(sed -n 's|^module .*/||p' go.mod)"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# The file name of the plain program archive (archive-programs.sh) of the system $1, written
# <os>-<arch>. The version is the one the Makefile made, without the v.
archive_name() {
    printf '%s-%s-%s.tar.gz\n' "$name" "${VERSION#v}" "$1"
}

# Prints the systems make build-all put in the folder $1/bin, one <os>-<arch> to a line. The
# build decides what the release is made for, so the files that are made from its output ask
# it here and keep no list of their own.
built_systems() {
    [ -d "$1/bin" ] || fail "missing $1/bin, run make build-all first"
    found="$(cd "$1/bin" && LC_ALL=C ls)"
    [ -n "$found" ] || fail "$1/bin holds no system, run make build-all first"
    echo "$found"
}
