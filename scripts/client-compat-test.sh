#!/bin/sh
# Tests scripts/client-compat-lib.sh, which decides the parts of the integration test from the
# client's own scripts. Runs on the host, no Docker, no network (make client-compat-test, part
# of make unit). The clients are small hand-made folders.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
failed=0

# shellcheck source=client-compat-lib.sh
. "$root/scripts/client-compat-lib.sh"

# $1 case name, $2 expected output, then the command. The status of the command is part of
# the output, so that a failure is as visible as a wrong list.
check() {
    case_name="$1"
    want="$2"
    shift 2
    status=0
    got="$("$@" 2>&1)" || status=$?
    got="$got [status $status]"
    if [ "$got" = "$want" ]; then
        printf 'ok   %s\n' "$case_name"
    else
        printf 'FAIL %s: want "%s", got "%s"\n' "$case_name" "$want" "$got"
        failed=$((failed + 1))
    fi
}

# A client that has parts: its compat.sh knows INTEGRATION_PART and lists them.
mkdir -p "$work/with-parts/scripts"
cat >"$work/with-parts/scripts/compat.sh" <<'EOF'
#!/bin/sh
# INTEGRATION_PART=compat runs only the compatibility runs.
if [ "${1:-}" = parts ]; then
    printf 'compat\ncluster-1\ncluster-2\n'
    exit 0
fi
echo "the whole test ran"
EOF

# A client from before parts: asking its compat.sh for parts would start its whole test.
mkdir -p "$work/old/scripts"
cat >"$work/old/scripts/compat.sh" <<'EOF'
#!/bin/sh
echo "the whole test ran"
EOF

# A client from before compat.sh.
mkdir -p "$work/oldest/scripts"

check "lists the parts the client lists" "compat
cluster-1
cluster-2 [status 0]" client_parts "$work/with-parts"
check "a client without parts is one part, all, and its test is not started" "all [status 0]" \
    client_parts "$work/old"
check "a client without compat.sh is one part, all" "all [status 0]" client_parts "$work/oldest"

check "an empty part is no condition" " [status 0]" require_part "" "$work/with-parts"
check "accepts a part the client lists" " [status 0]" require_part cluster-2 "$work/with-parts"
check "refuses a part the client does not list, and says which it has" \
    "client-compat.sh: INTEGRATION_PART=cluster-9 is not a part of the integration test; the parts are: compat cluster-1 cluster-2  [status 1]" \
    require_part cluster-9 "$work/with-parts"
check "refuses a part that is only the start of one" \
    "client-compat.sh: INTEGRATION_PART=cluster is not a part of the integration test; the parts are: compat cluster-1 cluster-2  [status 1]" \
    require_part cluster "$work/with-parts"
check "accepts all from a client without parts" " [status 0]" require_part all "$work/old"
check "refuses all from a client that has parts" \
    "client-compat.sh: INTEGRATION_PART=all is not a part of the integration test; the parts are: compat cluster-1 cluster-2  [status 1]" \
    require_part all "$work/with-parts"

# Prints yes or no: whether part $1 includes step $2.
includes() {
    answer=no
    # part_includes is one test whose status is the answer, so set -e being off inside it changes nothing.
    # shellcheck disable=SC2310
    if part_includes "$2" "$1"; then answer=yes; fi
    echo "$answer"
}
check "an empty part includes the compat runs" "yes [status 0]" includes "" compat
check "an empty part includes the cluster scenarios" "yes [status 0]" includes "" cluster
check "compat includes the compat runs" "yes [status 0]" includes compat compat
check "compat does not include the cluster scenarios" "no [status 0]" includes compat cluster
check "a group includes the cluster scenarios" "yes [status 0]" includes cluster-1 cluster
check "a group does not include the compat runs" "no [status 0]" includes cluster-1 compat

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "client compat parts: all cases as expected"
