# Changelog

Every merge into `main` is a release with its own version. This file has one heading per
version, newest first, and says what that version changed for the people who install and run
the SpliceFS server, `splicefsd`.

The change log of all parts of SpliceFS, side by side: https://splicefs.org/changelog/

## [0.5.7] - 2026-11-14

- Internal: a failed release, merge or scheduled check opens an issue that emails the owner. Nothing
  changes for users.
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
