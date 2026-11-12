# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Repository setup: design and configuration documents, contribution process, CI.
- `splicefsd --version`.
- HTTP API version 1 with health, login, node list, directory listings, file reads by byte
  range, head digests and free space; user files with argon2id password hashes; signed
  tokens with key rotation; login rate limiting; optional TLS; `splicefsd hash-password`.
- A systemd unit for `splicefsd`.
- Packages named `splicefs-server` for amd64 and arm64: deb (Debian, Ubuntu), rpm (Red Hat
  family) and apk (Alpine). Each installs the program and the service for its system
  (systemd, or OpenRC on Alpine), creates the `splicefs` user and an empty
  `/etc/splicefs/server`. The service is enabled and starts once a configuration exists.
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

### Fixed

- A request for a name longer than the file system allows is answered with 404 instead of
  500. Found by the new fuzz tests.
