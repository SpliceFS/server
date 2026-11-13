# Changelog

All notable changes to this project are recorded here. Every merge into `main` is a
release with its own version, so there are no version headings: the
[release page](../../releases) of a version names its change, and this file collects the
notable ones.

## Changes

### Added

- Repository setup: design and configuration documents, contribution process, CI.
- `splicefsd --version`.
- HTTP API version 1 with health, login, node list, directory listings, file reads by byte
  range, head digests and free space; user files with argon2id password hashes; signed
  tokens with key rotation; login rate limiting; optional TLS; `splicefsd hash-password`.
- A systemd unit for `splicefsd`.
- Packages named `splicefs-server` for amd64 and arm64: deb (Debian, Ubuntu), rpm (Red Hat
  family) and apk (Alpine). Each installs the program and the service for its system
  (systemd, or OpenRC on Alpine), creates the `splicefs` user and the empty folders
  `conf.d` and `shares.d` in `/etc/splicefs/server`. The service is enabled and started on
  install, and an upgrade never touches the files an administrator put in those folders.
- `make package-test` install-tests the packages on every supported Debian, Ubuntu, Rocky
  Linux, Amazon Linux, openSUSE and Alpine version, listed in `packaging/test-systems`.
- Signed apt, rpm and Alpine repositories built from the packages (`make repo`), with a test
  that installs from each on every supported system with signature checks on
  (`make repo-test`).
- Fuzz tests for the configuration, user file, key file, token and password hash parsing, the
  mapping of request names to files in a share, and the HTTP request parsing, with `make fuzz`
  to run them.

- Release and publish: a `vX.Y.Z` tag builds, signs, tests and publishes the package
  repositories to `packages.splicefs.org/server` (`make publish-fetch`, `make publish`,
  `make publish-check`) and creates a GitHub release with the packages, `SHA256SUMS` and its
  signature (`make release-assets`). Pre-release tags such as `v1.2.0-rc.1` give versions that
  sort before the release in deb, rpm and apk; `make publish-test` checks the publishing
  without a server.

- The server runs with nothing configured: no shares, no users, listening on every interface
  on port 8480 with TLS, using a certificate it creates itself (ECDSA P-256, valid for 10
  years). A listener can use its own certificate with `tls`, or serve without TLS with
  `plain: true` behind a reverse proxy.
- `splicefsd config check` validates the configuration and prints every problem with the file
  it came from. `splicefsd config show` prints the effective configuration with the file each
  value came from. Both take `--config DIR`.
- `SIGHUP` applies the configuration without a restart (`systemctl reload splicefsd`,
  `rc-service splicefsd reload`). Requests in progress finish on the configuration they
  started with, ports that stay configured are never closed, certificates are read again, and
  a configuration with an error is refused and only logged. Only `log.format` needs a restart.
- Default folders for a macOS build: `~/.splicefs/server` for the configuration and
  `~/.splicefs/var/server` for the state. `--config DIR` and `--state DIR` override the
  defaults on every system.
- Trunk development: one branch, `main`, and one version per change.
- `make service-test`: on every supported system the package is installed under that system's
  real service manager (systemd or OpenRC as process 1 of a container) and the service must
  start by itself, come back after a reboot and stop when the package is removed. CI also
  installs the package on the runner itself (`make service-test-host`).
- FreeBSD package: `make package-freebsd` builds `splicefs-server` for FreeBSD 14 and 15, on
  amd64 and arm64, with `pkg` on FreeBSD itself (a virtual machine per release), installs it
  there and checks that the service runs.
- `make check` fails when a tracked file names a private machine, account or place, and the
  ignore rules cover programs built in place, editor leftovers and disk images.
- FreeBSD packages are published as a signed package repository
  (`https://packages.splicefs.org/server/freebsd/${ABI}`).

### Changed

- **Breaking:** the configuration is a set of files instead of one `config.yaml`. Settings go
  in `conf.d/*.yaml`, read in name order and merged, and each share is one file in
  `shares.d/` whose file name is the share name. The settings `shares`, `auth.users_dir`,
  `auth.keys_file` and `cluster.nodes_file` are gone. A configuration written for an earlier
  build must be rewritten.
- **Breaking:** the server keeps its data in the state folder (`/var/lib/splicefs/server` in
  Linux packages), split into `local/` for this machine only (node id, generated
  certificate) and `shared/`, which may be synced between servers with any file sync tool.
  `shared/` holds `users/`, written by the administrator, and `keys/` and `nodes/`, one file
  per server that only that server writes. The server creates its own signing key and node
  entry. The single keys file and node list file no longer exist.
- Packages start the service on install even when nothing is configured.

### Fixed

- A request for a name longer than the file system allows is answered with 404 instead of
  500. Found by the new fuzz tests.
- FreeBSD: `make check-freebsd` runs the tests as root in a FreeBSD virtual machine, with the
  100% coverage gate, and CI runs it for FreeBSD 14 and 15. The FreeBSD scripts are the ones
  the client uses.
- A user file that cannot be read as YAML is reported without quoting any of its content, so a
  misplaced password hash cannot reach a log.
- `make lint` also runs ShellCheck, with every optional check on; the two that are off say why
  in `.shellcheckrc`. The scripts no longer hide the failure of a command inside `$(...)`.

### Removed

- openSUSE Leap 15.6 is no longer supported: its vendor ended its support on 2026-04-30. A
  system is supported only while its own vendor supports it.
- A compiled copy of the program that was committed by mistake is removed from the repository,
  and `make check` now fails when a compiled program or a file over 1 MiB is tracked.
- Code quality checks at their strictest: every linter is on, and each one that is off says
  why in `.golangci.yml`. The code declares its errors, so callers can test for
  them. Behavior, the HTTP interface, the configuration keys and the messages are unchanged.

