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
