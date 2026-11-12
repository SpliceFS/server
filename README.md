# SpliceFS server

[![CI](https://github.com/SpliceFS/server/actions/workflows/ci.yml/badge.svg)](https://github.com/SpliceFS/server/actions/workflows/ci.yml)
[![Security](https://github.com/SpliceFS/server/actions/workflows/security.yml/badge.svg)](https://github.com/SpliceFS/server/actions/workflows/security.yml)
[![Coverage](https://codecov.io/gh/SpliceFS/server/graph/badge.svg)](https://codecov.io/gh/SpliceFS/server)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/SpliceFS/server/badge)](https://scorecard.dev/viewer/?uri=github.com/SpliceFS/server)
[![Go Reference](https://pkg.go.dev/badge/github.com/SpliceFS/server.svg)](https://pkg.go.dev/github.com/SpliceFS/server)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/16604/badge)](https://www.bestpractices.dev/projects/16604)
[![Quality gate](https://sonarcloud.io/api/project_badges/measure?project=SpliceFS_server&metric=alert_status)](https://sonarcloud.io/summary/overall?id=SpliceFS_server)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

SpliceFS mounts a read-only folder whose files are streamed from several servers at once.
Every server that holds a copy of a file serves blocks of it, and the client splices the
blocks back together in order, caching them on local disk.

It is built for throughput: a reader far away from every server gets the combined upload
speed of all of them, and a slow or failing server is routed around while a file is being read.

This repository holds the server, `splicefsd`. The client that mounts the files, `splicefs`, is in
the [client repository](https://github.com/SpliceFS/client).

> **Status: alpha.** The server and the client work end to end on Linux and macOS and are
> covered by tests, but nothing has been run in production yet and no release has been
> tagged. The API and the configuration format may still change. Read the
> [known limitations](docs/design.md#known-limitations) first.

## How it works

```
                +------------------+
                |  node: east      |  files on local disk
            +-->|  splicefsd       |
            |   +------------------+
+--------+  |   +------------------+
| reader |--+-->|  node: west      |  same files, kept in step by any
| mount  |  |   |  splicefsd       |  replication tool you already use
+--------+  |   +------------------+
            |   +------------------+
            +-->|  node: ...       |
                |  splicefsd       |
                +------------------+
```

- **Server (`splicefsd`)** runs on each node and serves its local directories over HTTP:
  directory listings as JSON and file contents by byte range. It keeps no state about readers.
- **Client (`splicefs`)**, in its [own repository](https://github.com/SpliceFS/client), asks every
  node what it has, merges the answers into one tree and mounts it with FUSE. When a file is read,
  it requests blocks from all nodes that hold an identical copy, gives more work to the faster
  nodes, and requests a late block again from another node.
- **Replication is not part of SpliceFS.** Use Syncthing, rsync, a clustered filesystem, or
  nothing at all: a cluster of one node works. SpliceFS only requires that a file with the
  same path, size and modification time has the same content on every node.

## Features

- The server only reads: it never writes to a share
- No state about readers and no database
- Logins with per-user files and signed, short-lived tokens that any node can issue and check
- Works behind a reverse proxy or stands alone with its own TLS
- Plain HTTP API: every request can be reproduced with `curl`

## Quick start

On every node, with the files already in place under `/srv/media`:

```
# /etc/splicefs/server/config.yaml
listen:
  - {address: 0.0.0.0, port: 8480}
shares:
  media: {path: /srv/media}
cluster: {name: example, node: east, nodes_file: /srv/shared/nodes.yaml}
auth: {users_dir: /srv/shared/users, keys_file: /srv/shared/keys.yaml}
```

The node list, the users directory and the keys file are the same on every node; copy or
replicate them however you like. Create them once:

```
# /srv/shared/nodes.yaml
cluster: example
nodes:
  - {name: east, url: "https://east.example.com/api"}
  - {name: west, url: "https://west.example.com/api"}

# /srv/shared/keys.yaml  (mode 600)
keys:
  - {id: "2026-11", secret: "<output of: head -c 48 /dev/urandom | base64>"}

# /srv/shared/users/alice.yaml
password_hash: "<output of: splicefsd hash-password>"
shares: [media]
```

Start `splicefsd` on each node (a systemd unit is in `packaging/systemd`). To mount the files on
a reader, set up the [client](https://github.com/SpliceFS/client).

The [configuration reference](docs/configuration.md) lists every setting.

## Documentation

- [Design](docs/design.md): architecture, API, consistency rules
- [Configuration](docs/configuration.md): server configuration reference
- [Client repository](https://github.com/SpliceFS/client): the client, `splicefs`, with its own
  design and configuration documents
- [Contributing](CONTRIBUTING.md): development setup, standards, how changes are reviewed
- [Changelog](CHANGELOG.md)
- [Security policy](SECURITY.md)

## Requirements

`splicefsd` is a single static binary with every library built in; there is nothing to install
on any system.

## Building

Requires Go 1.26 or newer.

```
make build      # the binary is ./bin/splicefsd
make check      # everything CI runs: format check, vet, tests with the race detector, coverage gate
make check-linux  # the same on Linux, in a container (needs Docker)
make lint         # golangci-lint
make package      # deb, rpm and apk packages for amd64 and arm64 in ./dist (needs Docker)
make package-test # installs and removes each package in a clean container of every system
make repo         # signed apt, rpm and Alpine repositories in ./dist/repo from the packages in ./dist
make repo-test    # installs from the repositories with signature checks on, in every system
```

`make package-test` covers every system listed in `packaging/test-systems`. From an arm64
machine, `make package-test PLATFORM=linux/amd64` tests the other CPU type by emulation.

## Repository layout

```
cmd/splicefsd/  the program: command line and the hash-password command
app/            builds every part from the configuration and serves HTTP
config/         loads and validates config.yaml
api/            the HTTP API, version 1
auth/           password hashes, user files, signed tokens, login rate limiter
share/          safe read-only access to one shared directory
cluster/        the node list file
internal/       the version number set at build time
docs/           design and configuration reference
packaging/      systemd unit
scripts/        development helpers used by make and CI
```

The JSON types of the HTTP API (`wire`) and the size and duration types of the configuration
(`units`) come from the [common](https://github.com/SpliceFS/common) module, which the client
uses too.

## Versioning

SpliceFS follows [Semantic Versioning](https://semver.org). Releases are git tags of the form
`vMAJOR.MINOR.PATCH` in this repository, and `splicefsd --version` prints the version. The
server and the client are versioned separately: each repository has its own version tags. A
client works with a server that serves the API version the client speaks, and that version is
part of the URL (`/v1/`). Before 1.0.0 the API and the configuration format may change
in any minor release; every change is listed in the changelog.

## License

MIT. See [LICENSE](LICENSE).
