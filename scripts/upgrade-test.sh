#!/bin/sh
# Tests the upgrade from the newest published version to the one in dist/, but only when the
# packaging changed: a version whose packages differ from the published ones only in the
# program (and the changelog) cannot break an upgrade, so there is nothing to test. Runs on
# the host after make package (make upgrade-test, CI). For each package format, for this
# machine's CPU type, it
#   1. downloads the newest published package of that format from the GitHub release into
#      dist/published,
#   2. lists the files, scripts and declarations of the published and the new package inside
#      a container of the format's first system in packaging/test-systems, and compares the
#      two lists (scripts/upgrade-diff.sh),
#   3. when they differ, installs the published package in a clean container, writes a
#      configuration line and a data file, installs the new package over it the way a user
#      upgrades, and checks that both survived, the service user exists, the new version is
#      installed and every file the new package lists is there (scripts/upgrade-test-<format>.sh).
# Environment: VERSION and PACKAGE_DATE as for the build; PLATFORM (linux/amd64 or
# linux/arm64) tests that CPU type by emulation, empty means this machine's own. It keeps going
# after a failure so one run shows every format that is broken.
set -eu

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
# package-lib.sh reads files relative to the repository root.
cd "$root"
. scripts/package-lib.sh

releases="https://github.com/SpliceFS/server/releases"
platform="${PLATFORM:-$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')}"
case "$platform" in
    linux/amd64) deb_arch=amd64; rpm_arch=x86_64; apk_arch=x86_64 ;;
    linux/arm64) deb_arch=arm64; rpm_arch=aarch64; apk_arch=aarch64 ;;
    *) fail "no package CPU type for $platform" ;;
esac

# Both downloads insist on HTTPS, also after a redirect, so that a redirect can never hand
# over a package from a plain address.
# The tag of the newest release. GitHub answers with the releases page itself when there is
# none; a network failure is a failure of this script and not a reason to skip.
final_url="$(curl -fsSIL --proto =https --proto-redir =https --retry 3 -o /dev/null -w '%{url_effective}' "$releases/latest")" ||
    fail "could not ask $releases/latest for the newest release"
case "$final_url" in
    */releases/tag/*) tag="${final_url##*/}" ;;
    *) tag="" ;;
esac

# What the published files are called is what package-lib.sh makes of the tag.
published_dir="$root/dist/published"
if [ -n "$tag" ]; then
    names="$(VERSION="$tag" PACKAGE_DATE="$PACKAGE_DATE" sh -c \
        '. scripts/package-lib.sh; echo "$debver $rpmver $apkver"')" ||
        fail "could not work out the file names of the published version $tag"
    read -r old_debver old_rpmver old_apkver <<LIST
$names
LIST
fi

log="$(mktemp)"
old_list="$(mktemp)"
new_list="$(mktemp)"
trap 'rm -f "$log" "$old_list" "$new_list"' EXIT
passed=0
skipped=0
failed=0

# Runs scripts/upgrade-test-<format>.sh in a clean container of $image with the repository
# at /src and dist at /out, both read-only. Arguments: the script name, then its own.
in_container() {
    script="$1"
    shift
    docker run --rm --platform "$platform" -e VERSION -e PACKAGE_DATE \
        -v "$root":/src:ro -v "$root/dist":/out:ro -w /src "$image" sh "scripts/$script" "$@"
}

for format in deb rpm apk; do
    # The first system of the format is the one tested, the same as for the other tests.
    image="$(awk -v f="$format" '$1 == f { print $2; exit }' packaging/test-systems)"
    [ -n "$image" ] || fail "packaging/test-systems has no $format system"
    if [ -z "$tag" ]; then
        echo "skip $format: no published version yet, nothing to upgrade from"
        skipped=$((skipped + 1))
        continue
    fi
    case "$format" in
        deb)
            old_file="${name}_${old_debver}_$deb_arch.deb"
            new_file="${name}_${debver}_$deb_arch.deb"
            ;;
        rpm)
            old_file="$name-$old_rpmver-1.$rpm_arch.rpm"
            new_file="$name-$rpmver-1.$rpm_arch.rpm"
            ;;
        *)
            old_file="$name-$old_apkver-r0.$apk_arch.apk"
            new_file="alpine/$apk_arch/$name-$apkver-r0.apk"
            ;;
    esac
    [ -f "dist/$new_file" ] || fail "dist/$new_file does not exist; run make package first"

    mkdir -p "$published_dir"
    if [ ! -f "$published_dir/$old_file" ]; then
        # Written under another name first, so an interrupted download is never mistaken
        # for the package.
        if ! curl -fsSL --proto =https --proto-redir =https --retry 3 -o "$published_dir/$old_file.part" "$releases/download/$tag/$old_file" ||
            ! mv "$published_dir/$old_file.part" "$published_dir/$old_file"; then
            rm -f "$published_dir/$old_file.part"
            echo "FAIL $format: could not download $releases/download/$tag/$old_file"
            failed=$((failed + 1))
            continue
        fi
    fi

    # A registry failure must not look like a failed upgrade.
    # Errexit is off for the call to read its status; pull_image checks every command itself.
    set +e
    pull_image "$platform" "$image"
    pulled=$?
    set -e
    if [ "$pulled" -ne 0 ]; then
        echo "FAIL $format $image: could not download the image (3 tries)"
        failed=$((failed + 1))
        continue
    fi

    set +e
    in_container "upgrade-test-$format.sh" list "/out/published/$old_file" > "$old_list" 2> "$log"
    listed=$?
    if [ "$listed" -eq 0 ]; then
        in_container "upgrade-test-$format.sh" list "/out/$new_file" > "$new_list" 2> "$log"
        listed=$?
    fi
    set -e
    if [ "$listed" -ne 0 ]; then
        echo "FAIL $format $image: could not read the packages"
        tail -n 15 "$log" | sed 's/^/     /'
        failed=$((failed + 1))
        continue
    fi

    # The program and the changelog change in every version and are not compared.
    set +e
    differences="$(sh scripts/upgrade-diff.sh "$old_list" "$new_list" "$program" CHANGELOG.md)"
    compared=$?
    set -e
    if [ "$compared" -eq 0 ]; then
        echo "$format: only the program changed; no upgrade test needed"
        passed=$((passed + 1))
        continue
    fi
    if [ "$compared" -ne 1 ]; then
        echo "FAIL $format: could not compare the packages"
        failed=$((failed + 1))
        continue
    fi
    echo "$format: the packaging changed since $tag, running the upgrade test:"
    printf '%s\n' "$differences"

    set +e
    in_container "upgrade-test-$format.sh" upgrade "/out/published/$old_file" "/out/$new_file" > "$log" 2>&1
    upgraded=$?
    set -e
    if [ "$upgraded" -eq 0 ]; then
        echo "ok   $format $image: $tag upgraded to $VERSION"
        passed=$((passed + 1))
    else
        echo "FAIL $format $image"
        tail -n 15 "$log" | sed 's/^/     /'
        failed=$((failed + 1))
    fi
done

echo "passed: $passed  skipped: $skipped  failed: $failed"
[ "$failed" -eq 0 ]
