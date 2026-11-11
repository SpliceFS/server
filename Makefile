VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/SpliceFS/server/internal/version.version=$(VERSION)

# Image for running the Linux checks from another system.
LINUX_IMAGE ?= golang:1.27
LINUX_RUN := docker run --rm -v "$(CURDIR)":/src:ro -v splicefs-gomod:/go/pkg/mod $(LINUX_IMAGE)

.PHONY: all build check check-linux fmt fmt-check vet test cover lint clean

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/splicefsd ./cmd/splicefsd

# Everything CI runs except the linter.
check: fmt-check vet cover

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt formatted:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race ./...

# Tests with the race detector; fails below 100% statement coverage.
cover:
	scripts/coverage.sh coverage.out

# The same checks on Linux, from any system that has Docker.
# The source is copied inside the container so nothing is written to the working tree.
check-linux:
	$(LINUX_RUN) sh -c 'cp -r /src /tmp/w && cd /tmp/w && make check'

lint:
	golangci-lint run

clean:
	rm -rf bin coverage.out coverage.html
