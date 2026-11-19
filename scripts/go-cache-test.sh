#!/bin/sh
# Test of the step in .github/actions/go-cache that names the cache entry (make go-cache-test,
# part of make check, no Docker). The step is shell, and a runner is the only place it ran, so a
# mistake in it showed up as an empty "key" input of actions/cache, in every job of every pull
# request. The test runs the real step, with the ${{ }} expressions filled in and a make that
# names an image, and reads what the step wrote to GITHUB_OUTPUT.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
action="$root/.github/actions/go-cache/action.yml"
tree="$(mktemp -d)"
trap 'rm -rf "$tree"' EXIT
failed=0

# A real go.sum hash starts with digits and letters, and the step appends it to the prefix.
hash=592b79a05ad6314345a5805cbd3fc9897569d24da0361bfb5f43db717ab40358

# The body of the run: block of the step with the id "cache", without its indentation.
awk '
    /^    - /                  { inside = 0 }
    /^      id: cache$/        { inside = 1 }
    inside && /^      run: \|$/ { body = 1; next }
    body && /^        /        { sub(/^        /, ""); print; next }
    body                       { exit }
' "$action" > "$tree/raw.sh"

sed -e "s/\${{ github.job }}/scan/" \
    -e "s/\${{ runner.os }}/Linux/" \
    -e "s/\${{ runner.arch }}/X64/" \
    -e "s/\${{ hashFiles('go.sum') }}/$hash/" \
    "$tree/raw.sh" > "$tree/step.sh"

mkdir "$tree/bin"
printf '#!/bin/sh\necho registry.example/go:v1\n' > "$tree/bin/make"
chmod 755 "$tree/bin/make"

if grep -qF "\${{" "$tree/step.sh" || [ ! -s "$tree/step.sh" ]; then
    echo "FAIL the step was not found, or has an expression this test does not fill in"
    exit 1
fi

# $1 the folder input. Prints the file the step wrote its outputs to.
run_step() {
    : > "$tree/out"
    PATH="$tree/bin:$PATH" FOLDER="$1" GITHUB_OUTPUT="$tree/out" bash --noprofile --norc -e -o pipefail "$tree/step.sh"
    cat "$tree/out"
}

# $1 case name, $2 the wanted lines of the output (whole lines, in this order), $3 the folder input.
expect() {
    out="$(run_step "$3")"
    case "
$out
" in
        *"
$2
"*) printf 'ok   %s\n' "$1" ;;
        *)
            printf 'FAIL %s: wanted\n%s\n     got\n%s\n' "$1" "$2" "$out"
            failed=$((failed + 1))
            ;;
    esac
}

prefix=go-scan-Linux-X64-registry.example/go:v1-
expect "the prefix names job, system, architecture and image" "prefix=$prefix" .cache
expect "the key is the prefix and the hash of go.sum" "key=$prefix$hash" .cache
expect "the paths are the two caches of the folder" "$(printf 'path<<END_OF_PATHS\n.cache/gomod\n.cache/gobuild\nEND_OF_PATHS')" .cache

[ "$failed" -eq 0 ] || { echo "$failed check(s) failed"; exit 1; }
echo "go-cache checks done"
