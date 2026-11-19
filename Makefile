# The version this commit is, or will be, released as: the file VERSION, which the release bot
# sets on every pull request. Git history is not read for it, because CI checks out one commit
# without tags. -dirty marks uncommitted changes to tracked files; no git checkout is clean.
# This is the one place that works it out: the scripts receive it as VERSION.
VERSION ?= v$(shell cat VERSION)$(if $(shell git status --porcelain --untracked-files=no 2>/dev/null),-dirty)
LDFLAGS := -s -w -X github.com/SpliceFS/server/internal/version.version=$(VERSION)

# The tools are in our images (ghcr.io/splicefs) and nothing of ours is: the source is mounted
# into the image when a part runs. The Go image runs the checks and the build.
LINUX_IMAGE ?= ghcr.io/splicefs/go:v1
# The image of a FreeBSD release, which runs the FreeBSD machine of that release: a FreeBSD disk
# that has been booted once and has pkg set up (amd64 only), with QEMU. @RELEASE@ is where the
# release goes; the releases are listed in freebsd-test/releases.
FREEBSD_IMAGE ?= ghcr.io/splicefs/freebsd-@RELEASE@:v2
# The date of the commit, not of the build: the time of every file in the release archives, so
# that a rebuild of a commit gives the same bytes.
BUILD_DATE ?= $(shell git log -1 --format=%cd --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d)
# What the scripts that make the release files read. They take these as environment
# variables; env sets them in an image that has no make.
BUILD_VARS = VERSION=$(VERSION) BUILD_DATE=$(BUILD_DATE)

# One runner per image: docker run with the source at /src, as the caller's own user, so that
# every file a part writes belongs to the caller and not to root. The Go caches are folders
# under CACHE, mounted where the images keep them; a named Docker volume is made by root,
# which a user other than root cannot write to.
CACHE ?= $(CURDIR)/.cache
CACHE_DIRS := $(CACHE)/gomod $(CACHE)/gobuild
RUN_USER = $(shell id -u):$(shell id -g)
DOCKER_RUN = docker run --rm --user "$(RUN_USER)" -v "$(CURDIR)":/src -w /src
# The cache paths are named here as well as in the go image, so that the run works with any
# Go image and any uid: HOME, GOPATH and GOCACHE all point at the mounted host folders.
LINUX_RUN = $(DOCKER_RUN) -v "$(CACHE)/gomod":/tmp/go/pkg/mod -v "$(CACHE)/gobuild":/tmp/go-cache \
	-e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
	-e GOLANGCI_LINT_CACHE=/tmp/go-cache/golangci-lint $(LINUX_IMAGE)

# Docker makes a folder that does not exist yet as root, so the caller makes them first.
$(CACHE_DIRS):
	mkdir -p $@

.PHONY: script-modes all build check check-in-images fmt fmt-check vet cover lint \
	freebsd-lib-test version-test part-test build-all-test fuzz-test release-assets \
	clean archives archive-programs-test release-assets-test workflow-timeout-test go-cache-test

all: check build

# Every folder in cmd is a program; each is written to bin under its own name.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

# The parts of a build. Each is a make target that runs its inner target (the same name with
# -here, the real work) in the image and prints how long it took (scripts/part.sh). A part
# runs alone on any machine that has Docker, and in CI, with the same command; parts that do
# not need each other can run at the same time. A -here target also runs on a machine that
# has the tools.
.PHONY: static static-checks static-here lint-go unit unit-here race race-here build-all build-all-here \
	fuzz fuzz-here fuzz-targets fuzz-packages

# gofmt, go vet, golangci-lint and ShellCheck, and the two checks that read the git index.
# Those run on the host and not in the image: a git worktree keeps its index outside the
# folder that is mounted.
static: | $(CACHE_DIRS)
	scripts/part.sh static $(MAKE) --no-print-directory static-checks

static-checks: script-modes tracked-files
	$(LINUX_RUN) make static-here

static-here: fmt-check vet tidy-check lint-shell lint-go

# go.mod and go.sum must be what go mod tidy writes, so that a dependency that is no longer
# used, or one that is used and not listed, is found in the pull request. -diff changes no file.
.PHONY: tidy-check
tidy-check:
	go mod tidy -diff

# golangci-lint for each system the code is built for, because the files of a system are only
# checked when that system is the target.
lint-go:
	@set -e; for os in linux darwin freebsd; do \
		echo "golangci-lint GOOS=$$os"; GOOS=$$os golangci-lint run; \
	done

# The tests with the 100% coverage gate, and the tests of the scripts that need no Docker.
unit: | $(CACHE_DIRS)
	scripts/part.sh unit $(LINUX_RUN) make unit-here

unit-here: cover freebsd-lib-test version-test part-test build-all-test \
	archive-programs-test release-assets-test fuzz-test workflow-timeout-test go-cache-test

# The same tests with the race detector, which makes them several times slower.
race: | $(CACHE_DIRS)
	scripts/part.sh race $(LINUX_RUN) make race-here

race-here:
	go test -race ./...

# Every program, for every system and CPU type, into ./dist/bin. The release archives read from
# there; nothing is compiled twice.
build-all: | $(CACHE_DIRS)
	scripts/part.sh build-all $(LINUX_RUN) make $(BUILD_VARS) build-all-here

build-all-here:
	LDFLAGS='$(LDFLAGS)' scripts/build-all.sh dist/bin

# The plain program archives in ./dist, one for each system and CPU type in ./dist/bin (make
# build-all): the programs, LICENSE and README.txt, for people who run SpliceFS by hand. They
# are the files of the GitHub release (make release-assets). Needs only tar and gzip.
archives:
	$(BUILD_VARS) scripts/archive-programs.sh

# The image the Go parts run in. CI keys its Go caches on it (.github/actions/go-cache), so
# that a new image starts with empty caches.
.PHONY: go-image
go-image:
	@echo $(LINUX_IMAGE)

# What a machine that has every tool runs: the three parts, natively.
check: script-modes tracked-files static-here unit-here race-here

# The same on any system that has Docker, in the images.
check-in-images: static unit race

# A script that lost its executable bit fails only where it is run directly, which is often a
# CI job. This finds it in every check. Libraries (-lib.sh) are
# read by other scripts and never run.
script-modes:
	@bad="$$(git ls-files -s -- 'scripts/*.sh' | awk '$$1 != "100755" && $$4 !~ /-lib\.sh$$/ {print $$4}')"; \
	if [ -n "$$bad" ]; then echo "not executable in git:"; echo "$$bad"; exit 1; fi

# Nothing that must stay private may be tracked: no compiled program (it carries the folder
# names of the machine it was built on), no file over 1 MiB, no name of a private machine,
# account or place. The ignore rules help; this check is what stops a mistake.
.PHONY: tracked-files
tracked-files:
	scripts/tracked-files.sh

# Not gofmt on the folder itself: that would also format the module cache in ./.cache. Hidden
# folders hold no source of this project.
GO_FILES = find . -name '*.go' -not -path './.*'

fmt:
	$(GO_FILES) | xargs gofmt -w

fmt-check:
	@out="$$($(GO_FILES) | xargs gofmt -l)"; if [ -n "$$out" ]; then echo "not gofmt formatted:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

# Tests with coverage; fails below 100% statement coverage. The race detector has a part of
# its own (race).
cover:
	scripts/coverage.sh coverage.out

# Every fuzz target, each for FUZZTIME one after another. FUZZ_PACKAGE=package limits the run
# to the targets of one package, FUZZ_TARGET=package:Target to one target; both can be given.
# make -s fuzz-targets prints the targets as package:Target lines and make -s fuzz-packages the
# packages that have any. They are found in the code, so a new Fuzz function needs no edit
# here. The Daily workflow starts one job for each package every day, with the same command. A
# failing input is written to testdata/fuzz in the package's folder.
FUZZTIME ?= 10s
FUZZ_PACKAGE ?=
FUZZ_TARGET ?=
fuzz: | $(CACHE_DIRS)
	scripts/part.sh fuzz $(LINUX_RUN) make FUZZTIME=$(FUZZTIME) FUZZ_PACKAGE=$(FUZZ_PACKAGE) \
		FUZZ_TARGET=$(FUZZ_TARGET) fuzz-here

fuzz-here:
	scripts/fuzz.sh $(FUZZTIME) $(FUZZ_PACKAGE) $(FUZZ_TARGET)

# Nothing but the lines on standard output, so that CI can read them (it asks with -s, which
# also keeps make from printing the folders it makes).
fuzz-targets: | $(CACHE_DIRS)
	@$(LINUX_RUN) scripts/fuzz-targets.sh

fuzz-packages: | $(CACHE_DIRS)
	@$(LINUX_RUN) scripts/fuzz-packages.sh

# Runs the tests on FreeBSD itself: the test programs are built for FreeBSD in the Go
# container and run as root in a virtual machine of every FreeBSD release in
# freebsd-test/releases, with the same 100% coverage gate as make check. FreeBSD cannot
# run in a container, so the machine runs under QEMU in the image of the release
# (FREEBSD_IMAGE). This needs Docker and KVM (Linux on amd64); a computer without /dev/kvm
# stops at once, because an emulated machine takes many times as long. FREEBSD=15.1 limits the run to
# one release.
FREEBSD ?=
FREEBSD_ONLY ?=
.PHONY: check-freebsd freebsd-kvm
check-freebsd: freebsd-kvm | $(CACHE_DIRS)
	$(LINUX_RUN) sh scripts/freebsd-tests.sh dist/freebsd-check
	LINUX_IMAGE=$(LINUX_IMAGE) FREEBSD_IMAGE=$(FREEBSD_IMAGE) FREEBSD=$(FREEBSD) \
		FREEBSD_ONLY=$(FREEBSD_ONLY) scripts/check-freebsd.sh dist

# First in every target that starts a FreeBSD machine, so that a computer that cannot is found
# before anything is built.
freebsd-kvm:
	scripts/freebsd-require-kvm.sh

# ShellCheck runs with every optional check on (see .shellcheckrc). The glob picks up a new
# script in scripts/ without editing this file.
lint: lint-shell
	golangci-lint run

# The shell part of lint by itself, for the CI job that has no Go tools.
.PHONY: lint-shell
lint-shell:
	shellcheck scripts/*.sh

# Where the version comes from: the file VERSION, also in a checkout of one commit without
# tags, which is what CI has. It needs no Docker, so it is part of check.
version-test:
	scripts/version-test.sh

# The scripts around the FreeBSD machines, against a stand-in for Docker. They need neither
# Docker nor a machine, so they are part of check and run on every system.
freebsd-lib-test:
	scripts/freebsd-lib-test.sh

# The helper that prints how long a part took, and the script that compiles every program.
# Neither needs Docker nor a compiler, so both are part of check.
part-test:
	scripts/part-test.sh

build-all-test:
	scripts/build-all-test.sh

archive-programs-test:
	scripts/archive-programs-test.sh

# The script that collects and signs the release files, with keys made for the test.
release-assets-test:
	scripts/release-assets-test.sh

# The scripts that find the fuzz targets and run them, against a stand-in for go.
fuzz-test:
	scripts/fuzz-test.sh

# The files of this version for the GitHub release, in dist/release: the program archives
# (make archives), SHA256SUMS and its signature. The environment names the OpenPGP signing key
# (SIGNING_KEY_FILE) and the folder with the public key it is checked against (KEYS_DIR); the
# release workflow sets both (.github/actions/release-keys). The packages are not released
# here: the package repository makes them from these archives.
release-assets:
	$(BUILD_VARS) scripts/release-assets.sh

# Every job of the workflows that runs on a runner has a time limit, so that a hang fails in
# minutes and not after the 6 hours GitHub allows by default. Needs no Docker.
workflow-timeout-test:
	scripts/workflow-timeout-test.sh

# The step of the go-cache action that names the cache entry runs only on a runner, so a test
# runs it here. Needs no Docker.
go-cache-test:
	scripts/go-cache-test.sh

clean:
	rm -rf bin dist coverage.out coverage.html
