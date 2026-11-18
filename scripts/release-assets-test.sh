#!/bin/sh
# Test of scripts/release-assets.sh in a private dist folder: the files it collects, that
# SHA256SUMS lists exactly the program archives of this version, and that it is signed (the
# script verifies the signature with the public key alone before it succeeds). The signing
# keys are made here and thrown away. Runs on
# the host, needs no Docker and compiles nothing (make release-assets-test, part of make
# unit). It needs gpg, as the script does; where gpg is missing it says SKIP, and the unit part
# of CI runs in an image that has it.
set -eu

if ! command -v gpg >/dev/null 2>&1; then
    echo "SKIP release-assets: gpg is not on the PATH"
    exit 0
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tmp="$(mktemp -d)"
gen="$tmp/gen"
cleanup() {
    GNUPGHOME="$gen" gpgconf --kill gpg-agent >/dev/null 2>&1 || true
    rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
failed=0
pass() { echo "ok   $*"; }
fail() {
    echo "FAIL $*"
    failed=$((failed + 1))
}

# The keys the way the release has them: the secret half holds only the signing subkey, and
# the public half is a file of its own. ed25519 keys are made in a moment.
keys="$tmp/keys"
mkdir -m 700 "$gen"
mkdir "$keys"
gpg_gen() { GNUPGHOME="$gen" gpg --batch --quiet --pinentry-mode loopback --passphrase '' "$@"; }
gpg_gen --quick-generate-key 'Test key <test@example.invalid>' ed25519 cert never 2>/dev/null
fingerprint="$(gpg_gen --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')"
gpg_gen --quick-add-key "$fingerprint" ed25519 sign never 2>/dev/null
gpg_gen --armor --export-secret-subkeys "$fingerprint" >"$keys/secret.asc" 2>/dev/null
gpg_gen --armor --export "$fingerprint" >"$keys/splicefs.asc" 2>/dev/null

# A private dist that holds one plain program archive for each system of its dist/bin, under
# the names release-lib.sh gives this pre-release version. One shell asks for all the names:
# every call of the library reads the repository again.
export VERSION=v1.2.3-rc.1 PACKAGE_DATE=20261112
d="$tmp/dist"
systems="darwin-amd64 darwin-arm64 freebsd-amd64 freebsd-arm64 linux-amd64 linux-arm64"
# The names are ours and hold no spaces, so the list is split on purpose.
# shellcheck disable=SC2086
names="$(sh -c '. scripts/release-lib.sh; echo "$name"; for s in "$@"; do archive_name "$s"; done' sh $systems)"
name="$(printf '%s\n' "$names" | sed -n 1p)"
for system in $systems; do mkdir -p "$d/bin/$system"; done
printf '%s\n' "$names" | sed 1d | while read -r archive; do echo "archive" >"$d/$archive"; done
# Another version in the same folder must not be collected, and neither must a package: the
# packages are made and published by the package repository, not by this release.
echo other >"$d/${name}-0.0.1-linux-amd64.tar.gz"
echo package >"$d/${name}_1.2.3~rc.1_amd64.deb"

if DIST_DIR="$d" KEYS_DIR="$keys" SIGNING_KEY_FILE="$keys/secret.asc" \
    sh scripts/release-assets.sh >"$tmp/out" 2>&1; then
    rel="$d/release"
    count="$(find "$rel" -type f | wc -l | tr -d ' ')"
    if [ "$count" -eq 8 ]; then pass "release-assets made 8 files"; else
        fail "expected 8 files, got $count"
        ls "$rel"
    fi
    if command -v sha256sum >/dev/null 2>&1; then sumcheck="sha256sum -c"; else
        sumcheck="shasum -a 256 -c"
    fi
    # sumcheck is a command and its option, so it is split on purpose.
    # shellcheck disable=SC2086
    if (cd "$rel" && $sumcheck SHA256SUMS >/dev/null 2>&1); then
        pass "SHA256SUMS verifies with $sumcheck"
    else fail "SHA256SUMS does not verify"; fi
    archives="$(grep -c "^[0-9a-f]*  $name-${VERSION#v}-.*\.tar\.gz\$" "$rel/SHA256SUMS" || true)"
    listed="$(wc -l <"$rel/SHA256SUMS" | tr -d ' ')"
    if [ "$archives" -eq 6 ] && [ "$listed" -eq 6 ]; then
        pass "SHA256SUMS lists the plain program archive of each of the 6 systems and nothing else"
    else fail "expected exactly the 6 program archives in SHA256SUMS, got $archives"; fi
    # The script verifies the signature against the public key alone before it succeeds, so the
    # signature file only has to be there.
    if [ -s "$rel/SHA256SUMS.asc" ]; then pass "SHA256SUMS.asc is there and the script verified it"
    else fail "SHA256SUMS.asc is missing"; fi
else
    fail "release-assets.sh"
    sed 's/^/     /' "$tmp/out"
fi

[ "$failed" -eq 0 ] || {
    echo "$failed check(s) failed"
    exit 1
}
echo "release-assets: the files of the release, the checksums and the signature are as expected"
