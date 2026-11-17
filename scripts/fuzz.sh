#!/bin/sh
# Usage: fuzz.sh FUZZTIME [PACKAGE:TARGET ...]
# Fuzzes each named target for FUZZTIME, or every target of fuzz-targets.sh when none is named.
# Go fuzzes one target per call, so this loops. A target that finds a failing input fails the
# script at once, and Go has written the input to testdata/fuzz in the package's folder.
set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: fuzz.sh FUZZTIME [PACKAGE:TARGET ...]" >&2
    exit 2
fi
fuzztime="$1"
shift

# Go passes a pattern that matches nothing with "no fuzz tests to fuzz", so a misspelled target
# would pass without fuzzing anything. Asking the package is cheap: its test binary is the one
# the fuzzing builds anyway.
check_known() {
    case "$1" in
    ?*:?*) ;;
    *)
        echo "$1 is not written as package:Target" >&2
        exit 2
        ;;
    esac
    listed="$(go test -list "^${1#*:}\$" "./${1%%:*}")"
    if ! printf '%s\n' "$listed" | grep -Fqx "${1#*:}"; then
        echo "$1 is not a fuzz target (make -s fuzz-targets lists them)" >&2
        exit 1
    fi
}

if [ "$#" -gt 0 ]; then
    for target in "$@"; do check_known "$target"; done
    targets="$*"
else
    targets="$("$(dirname "$0")/fuzz-targets.sh")"
fi

for target in $targets; do
    package="${target%%:*}"
    name="${target#*:}"
    echo "fuzz $package $name ($fuzztime)"
    go test -run '^$' -fuzz "^$name\$" -fuzztime "$fuzztime" "./$package"
done
