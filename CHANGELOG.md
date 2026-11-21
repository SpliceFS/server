# Changelog

Every merge into `main` is a release with its own version. This file has one heading per
version, newest first, and says what that version changed for the people who install and run
the SpliceFS server, `splicefsd`.

The change log of all parts of SpliceFS, side by side: https://splicefs.org/changelog/

## [0.8.0] - 2026-11-21

- Changed: login tokens are signed with each node's own Ed25519 key; the shared token key files are
  no longer used, and each server removes its own on start.

## [0.7.0] - 2026-11-21

- New: each node publishes its id and its public signing and encryption keys in its node file, and
  /v1/nodes lists them.

## [0.6.0] - 2026-11-21

- New: each node makes its own signing and encryption key pairs on its first start and keeps them in
  local/node-keys.yaml.

## [0.5.47] - 2026-11-19

- Internal: the server's pull requests no longer run the cross-repo integration jobs.

## [0.5.46] - 2026-11-19

- Internal: the server serves the API of the common library.

## [0.5.45] - 2026-11-19

- Internal: the digest store makes its own folder.

## [0.5.44] - 2026-11-19

- New: the server keeps file digests in its state folder, so they survive a restart.

## [0.5.43] - 2026-11-19

- Internal: a pull request tests the program itself, without packaging or fuzzing.

## [0.5.42] - 2026-11-19

- Internal: the start tests wait for the server to stop before they clean up.

## [0.5.41] - 2026-11-19

- Internal: every workflow job has a time limit.

## [0.5.40] - 2026-11-19

- Internal: packaging lives only in the packages repository.

## [0.5.39] - 2026-11-19

- Internal: the server's packages are built and tested by the packages repository.

## [0.5.38] - 2026-11-18

- Internal: a dependency is updated. Nothing changes for users.

## [0.5.37] - 2026-11-18

- New program `splicefsdctl` for the server's utility commands; `splicefsdctl hash-password`
  replaces `splicefsd hash-password`.

## [0.5.36] - 2026-11-18

- Internal: package upgrades are tested in the packages repository.

## [0.5.35] - 2026-11-18

- Internal: the server's packages are built and published by the packages repository; a server
  release publishes the programs.

## [0.5.34] - 2026-11-17

- Releases include the plain programs for every system and CPU type, listed in the signed checksums.

## [0.5.33] - 2026-11-17

- Internal: FreeBSD packaging reuses the built programs and takes its job template from the machine
  image.

## [0.5.32] - 2026-11-17

- Internal: the server's integration test with the released client runs in parallel parts.

## [0.5.31] - 2026-11-17

- Building from source needs Go 1.27 or newer.

## [0.5.30] - 2026-11-17

- Internal: package tests run on server images of each target system, with its service manager
  running.

## [0.5.29] - 2026-11-17

- Internal: the FreeBSD build removes the archive with the signing key as soon as the machine has
  run.

## [0.5.28] - 2026-11-17

- Internal: releases build in parallel and publish only after every build has finished.

## [0.5.27] - 2026-11-17

- Internal: fuzzing runs in the go image, one parallel CI job per package.

## [0.5.26] - 2026-11-17

- Internal: FreeBSD tests and packages run on prepared machine images, require hardware
  virtualization, and build the aarch64 packages in the amd64 machine.

## [0.5.25] - 2026-11-17

- Internal: every build and test part runs in prebuilt images as its own parallel CI job.

## [0.5.24] - 2026-11-16

- Internal: the FreeBSD package build runs every release again (prevents the failure the client
  release v0.5.27 had).

## [0.5.23] - 2026-11-16

- The node id is now a UUIDv7. A `local/node-id` file in the old 16-character form is refused;
  delete it and the server makes a new one.

## [0.5.22] - 2026-11-16

- Internal: builds take their version from the VERSION file, so pull request packages carry the
  version they will be released as.

## [0.5.21] - 2026-11-15

- Internal: a single system call reads everything that tells file versions apart, including the
  creation time.

## [0.5.20] - 2026-11-15

- A listener added by a configuration reload now logs "listening" with its address, as listeners do
  at startup.

## [0.5.19] - 2026-11-15

- Internal: the password tests hash at the production cost only once.

## [0.5.18] - 2026-11-15

- A file in a data directory that is replaced with one of the same size and modification time is now
  read again.

## [0.5.17] - 2026-11-15

- Internal: shutdown and configuration swap tests wait for real signals instead of time. Nothing
  changes for users.

## [0.5.16] - 2026-11-15

- Internal: contributors are told to merge `main` down to fix a branch the bot cannot rebuild.
  Nothing changes for users.

## [0.5.15] - 2026-11-15

- Internal: the FreeBSD test proves a reload was applied before it checks the service. Nothing
  changes for users.

## [0.5.14] - 2026-11-14

- Internal: the file identity code moves to its own code folder. Nothing changes for users.

## [0.5.13] - 2026-11-14

- Internal: an interrupted FreeBSD image unpack is no longer mistaken for a cached image. Nothing
  changes for users.

## [0.5.12] - 2026-11-14

- Internal: a release no longer opens a pull request in the client to move its shared code. Whoever
  changes the server or the client keeps common current by hand. Nothing changes for users.

## [0.5.11] - 2026-11-14

- Internal: a failed release, merge or scheduled check opens an issue that emails the owner. Nothing
  changes for users.
- The protocol names the server and client version headers and the digests a block request carries.

## [0.5.10] - 2026-11-14

- Internal: a change to the packaging is tested by upgrading from the last published version.
  Nothing changes for users.

## [0.5.9] - 2026-11-14

- The server notices a file replaced by another of the same size within the same second, and reports
  the new file's head and tail digests.

## [0.5.8] - 2026-11-14

- Internal: a failed release, merge or scheduled check opens an issue that emails the owner. Nothing
  changes for users.

## [0.5.7] - 2026-11-14

- The protocol names the server and client version headers and the digests a block request carries.

## [0.5.6] - 2026-11-14

- Internal: a release uploads the package files before the index files that name them, and stops at
  the first failed step. Nothing changes for users.

## [0.5.5] - 2026-11-14

- Internal: a release refuses a tag that is not on main or does not match `VERSION`. Nothing changes
  for users.

## [0.5.4] - 2026-11-14

- Internal: each test runs once per change, on the pull request. The checks on `main` and the tests
  inside the release are gone. Nothing changes for users.

## [0.5.3] - 2026-11-14

- Internal: a release of the shared code now opens the pull request that moves the server to it, and
  the server's release does the same for the client.

## [0.5.2] - 2026-11-14

- Internal: a release now updates the Homebrew formula and opens the client's change for a new
  version of the shared code. Nothing changes for users.

## [0.5.1] - 2026-11-14

- Internal: the checks now run the newest released client against the server with real programs, so
  a change of the server cannot break the clients that are out. Nothing changes for users.
- Internal: a release now waits for earlier releases to end. Nothing changes for users.

## [0.5.0] - 2026-11-14

- The server now sends a second digest of every file, taken from its last 64 KiB, in the header
  `X-SpliceFS-Tail-Digest`. Clients use it with size, time and the digest of the first 64 KiB to
  tell whether copies on different servers are the same file.
- Both digests are read in constant time when a file is first asked for, so a file that was just
  added to a share is usable at once.

## [0.4.0] - 2026-11-14

- Every response of the server now carries its version in the header `X-SpliceFS-Server-Version`. A
  client uses it to refuse a server that is older than it needs.
- The server moves to 0.4. With the same first version number, a client works with every server
  whose second number is equal or higher.

## [0.3.3] - 2026-11-13

- Documentation: the change log now has one entry per version, back to 0.1.0. All parts of SpliceFS
  are shown side by side at `splicefs.org/changelog`.

## [0.3.2] - 2026-11-13

- Documentation: the README and the configuration document now give the reload command for FreeBSD
  next to the ones for systemd and Alpine.

## [0.3.1] - 2026-11-13

- Internal: the package and service tests retry failed image downloads. Nothing changes for users.

## [0.3.0] - 2026-11-13

- The FreeBSD packages are now published as a signed repository at
  `packages.splicefs.org/server/freebsd/${ABI}`. Installing from it works with the signature check
  on, and a repository with another key or no signature is refused.

## [0.2.7] - 2026-11-12

- Internal: the repository checks now also fail when a tracked file names a private machine, account
  or place. Nothing changes for users.

## [0.2.6] - 2026-11-12

- Internal: the build, signing, test and publishing scripts get stricter checks. Nothing changes for
  users.

## [0.2.5] - 2026-11-12

- A user file that cannot be read as YAML is now reported with the file, the line and the kind of
  problem, without quoting anything from it. Before, a password hash under the wrong key could reach
  the server log and the output of `splicefsd config check`.

## [0.2.4] - 2026-11-12

- The FreeBSD package description no longer says that installing starts the server, which FreeBSD's
  `pkg` does not allow.

## [0.2.3] - 2026-11-12

- Internal: stricter code checks across the code base. Nothing changes for users.

## [0.2.2] - 2026-11-12

- Internal: a compiled copy of `splicefsd` committed by mistake is removed. Nothing changes for
  users.

## [0.2.1] - 2026-11-12

- openSUSE Leap 15.6 is no longer supported: its vendor ended support on 2026-04-30 and its
  repository signing key has expired. A system is supported only while its own vendor supports it.

## [0.2.0] - 2026-11-12

- New FreeBSD package `splicefs-server` for FreeBSD 14 and 15, on amd64 and arm64. It is not
  published yet.
- On FreeBSD the configuration is in `/usr/local/etc/splicefs/server` and the state in
  `/var/db/splicefs/server`. Installing does not start the service, which runs after the next boot;
  the install message names the command to start it now.

## [0.1.2] - 2026-11-12

- Internal: a new test checks the service on every supported system. Nothing changes for users.

## [0.1.1] - 2026-11-12

- Internal: the first release's publishing script was not executable and is now. Nothing changes for
  users.

## [0.1.0] - 2026-11-12

- First release of `splicefsd`, serving HTTP API version 1: login, node list, directory listings,
  file reads by byte range and free space. Users are files with argon2id password hashes (`splicefsd
  hash-password` makes one).
- It runs with nothing configured: no shares, no users, all interfaces on port 8480, TLS with a
  certificate it creates itself. A listener can use its own certificate with `tls`, or serve without
  TLS with `plain: true`.
- The configuration is files: settings in `conf.d/*.yaml` and one file per share in `shares.d/`.
  `splicefsd config check` and `splicefsd config show` print problems and effective values, and
  `SIGHUP` applies changes without a restart.
- Packages named `splicefs-server` for amd64 and arm64: deb, rpm and apk, with a systemd or OpenRC
  service and state in `/var/lib/splicefs/server`. Signed repositories are at
  `packages.splicefs.org/server`.
