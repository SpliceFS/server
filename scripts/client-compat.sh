#!/bin/sh
# Builds this server and the newest released client it must serve, and runs the client's own
# tests with both real programs against each other. Run it from anywhere; it works in the
# repository top. It needs Go, git, and what the client's tests need (Linux, root, FUSE).
#
# The rule it proves: with the same first version number, a client works with every server
# whose second number is equal or higher. The client repository tests each change of the
# client against the released servers. This script is the other direction: a change of this
# server must not break a client that is out. The client that is tested is the newest tag
# with the same first number and a second number that is equal or lower than the one this
# server is about to get. A client with a higher second number is not required to work with
# this server.
#
# CLIENT_TAG=v1.2.3 replaces that choice. CLUSTER_ONLY and CLUSTER_SEED, when set, reach the
# client's cluster test through the environment.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

# The version this server is about to get, as the release sets it in VERSION.
read -r version <VERSION
main="${version%%.*}"
rest="${version#*.}"
minor="${rest%%.*}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

client_tag="${CLIENT_TAG:-}"
if [ -z "$client_tag" ]; then
    # The tags of the client repository that are releases (v1.2.3), oldest first. sort -V is a
    # GNU option, so the numbers are sorted field by field instead.
    # Asked from the temporary folder: in a copy of a working tree whose git folder is
    # elsewhere, git would fail here before it ever looked at the address.
    remote="$(git -C "$tmp" ls-remote --tags --refs https://github.com/SpliceFS/client 'v*')" ||
        { echo "cannot list the tags of the client repository" >&2; exit 1; }
    tags="$(printf '%s\n' "$remote" |
        awk '{ sub("^refs/tags/", "", $2); if ($2 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) print substr($2, 2) }' |
        sort -t. -k1,1n -k2,2n -k3,3n |
        sed 's/^/v/')"
    client_tag="$(printf '%s\n' "$tags" |
        awk -F. -v main="$main" -v minor="$minor" \
            'substr($1, 2) == main && $2 + 0 <= minor + 0 { last = $0 } END { print last }')"
fi

if [ -z "$client_tag" ]; then
    echo "no released client that this server must serve; nothing to test"
    exit 0
fi

echo "server v$version against client $client_tag"

# The server carries the version it is about to get, so that it reports what a release reports.
go build -buildvcs=false \
    -ldflags "-X github.com/SpliceFS/server/internal/version.version=v$version" \
    -o "$tmp/splicefsd" ./cmd/splicefsd

git clone -q --depth 1 --branch "$client_tag" https://github.com/SpliceFS/client "$tmp/client"
cd "$tmp/client"
go build -buildvcs=false \
    -ldflags "-X github.com/SpliceFS/client/internal/version.version=$client_tag" \
    -o "$tmp/splicefs" ./cmd/splicefs

# The oldest clients that are tested have no cluster test yet; a script that the tag does not
# have is skipped, not an error.
run_client_script() {
    script="$1"
    shift
    if [ ! -f "scripts/$script" ]; then
        echo "client $client_tag has no scripts/$script; skipped"
        return 0
    fi
    "scripts/$script" "$@"
}

run_client_script compat-test.sh works "$tmp/splicefsd" "$tmp/splicefs"
run_client_script cluster-test.sh "$tmp/splicefsd" "$tmp/splicefs"

echo "client compat: all runs as expected"
