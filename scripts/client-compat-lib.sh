#!/bin/sh
# How the integration test is cut into parts. The client decides its parts; this server only
# asks. Sourced by client-compat.sh, and tested by client-compat-test.sh.

# Prints the parts of the client in folder $1, one per line. A client that lists its parts
# knows INTEGRATION_PART in its compat.sh. A client that does not would start its whole test
# when asked, so it is not asked, and its whole test is the one part, all.
client_parts() {
    if [ -f "$1/scripts/compat.sh" ] && grep -q INTEGRATION_PART "$1/scripts/compat.sh"; then
        sh "$1/scripts/compat.sh" parts
    else
        echo all
    fi
}

# Succeeds when part $1 is empty (the whole test) or one of the parts of the client in folder
# $2. Otherwise it says which parts there are and fails, before anything is built.
require_part() {
    if [ -z "$1" ]; then
        return 0
    fi
    list="$(client_parts "$2")"
    if ! printf '%s\n' "$list" | grep -qxF "$1"; then
        known="$(printf '%s\n' "$list" | tr '\n' ' ')"
        echo "client-compat.sh: INTEGRATION_PART=$1 is not a part of the integration test; the parts are: $known" >&2
        return 1
    fi
}

# Whether part $2 includes step $1 of the test, which is compat (the client's compatibility
# runs) or cluster (its cluster scenarios). The part compat is the first step alone, any
# other part is a group of the second, and an empty part is both.
part_includes() {
    case "$2" in
    '') return 0 ;;
    compat) [ "$1" = compat ] ;;
    *) [ "$1" = cluster ] ;;
    esac
}
