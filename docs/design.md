# SpliceFS design

This document is the specification the code is written against. Where the code and this
document disagree, one of them has a bug. Sections marked *open* are not decided yet.

## Goals

1. A reader mounts a read-only folder and gets each file from every node that holds it,
   at the combined speed of those nodes.
2. A slow or unreachable node costs throughput, never correctness and never a hung read.
3. The server side is simple enough to audit in an afternoon: stateless, plain HTTP, no
   database.
4. Nothing depends on a particular replication tool, DNS setup, proxy or number of nodes.
   One node is a valid cluster.

## Non-goals

- Writing through the mount.
- Replicating data between nodes.
- Low first-byte latency. The design favors sustained throughput.

## Terms

| Term | Meaning |
|---|---|
| node | one machine running the server, with the files on local disk |
| cluster | the set of nodes listed in one node list |
| share | a named directory a node offers |
| block | a fixed-size byte range of a file, the unit the client requests and caches |

## Server

The server (`splicefsd`) is an HTTP server. It reads files; it never writes to a share.

### API, version 1

All paths are below a configurable base path (default `/api`). Requests other than health
and login need `Authorization: Bearer <token>`.

| Method and path | Answer |
|---|---|
| `GET /v1/health` | `200` and `{"status":"ok","version":"..."}`. No login. For load balancers and DNS health checks. |
| `POST /v1/login` | Body `{"user":"...","password":"..."}`. `200` and `{"token":"...","expires":"<RFC 3339>"}`, or `401`. |
| `GET /v1/nodes` | `{"cluster":"...","nodes":[{"name":"...","url":"...","id":"...","sign_key":"...","encrypt_key":"..."}]}` from the files in `shared/nodes/`. |
| `GET /v1/tree/` | The shares this user may read, as directory entries. |
| `GET /v1/tree/<share>/<dir>/` | `{"entries":[{"name":"...","dir":false,"size":123,"mtime":"<RFC 3339>","etag":"..."}]}`, sorted by name. `404` if the directory does not exist on this node. |
| `GET`, `HEAD /v1/file/<share>/<path>` | The file. Supports `Range` (one range), `If-Range` and `If-Match`. Sends `ETag` and `Accept-Ranges: bytes`. `HEAD` also sends `X-SpliceFS-Head-Digest` and `X-SpliceFS-Tail-Digest` (see File identity). |
| `GET /v1/space/<share>` | `{"total":<bytes>,"free":<bytes>}` for the filesystem holding the share. |

Every response carries `X-SpliceFS-Server-Version` with the version of `splicefsd`. A client
uses it to refuse a server that is older than the client needs (same first number, second
number equal or higher).

Errors are JSON, `{"error":"..."}`, with the usual status codes: `400` malformed request,
`401` missing or invalid token, `403` share not allowed for this user, `404` not found,
`412` and `416` for failed preconditions and bad ranges, `429` login rate limit.

### What a node serves

- Regular files and directories inside a share. Requests are resolved inside the share's
  root; a path that leaves it, by `..` or by a symbolic link, is `404`.
- Entries matching a share's `exclude` patterns are neither listed nor served. The default
  excludes names starting with a dot, which covers the temporary files of most replication
  tools.
- Files modified more recently than the share's `min_age` are treated as absent. This
  protects against replication tools that write files in place. The default is zero, which
  is right for tools that write to a temporary name and rename.

### File identity

A file's tag (`ETag`) is derived from its size and its modification time, truncated to the
configured precision (default one second). Two nodes therefore produce the same tag for the
same file whenever the replication tool preserves modification times.

A `HEAD` request also returns two digests. `X-SpliceFS-Head-Digest` is the SHA-256 of the
first 64 KiB of the file. `X-SpliceFS-Tail-Digest` is the SHA-256 of the last 64 KiB, or of
the whole file when it is shorter, so for a file of at most 64 KiB the two are equal. Both
cost two small reads at fixed positions, however large the file is, so a file that was just
added is usable at once. The server keeps both per file until the file changes (see Digest
cache). They catch two different files that happen to share a size and a modification time
and differ at the start or at the end.

Two copies are treated as the same file when their tag, their head digest and their tail
digest are equal.

### Digest cache

A node keeps the head and tail digests of every file it has been asked about in its state
folder, so a restart or a reload does not read the files again. The cache is per node and
lives in `local/`: what decides whether an entry is still right (inode, change time) means
nothing on another node.

**Stamp.** One system call on the open file gives the stamp: `fstat` on macOS and FreeBSD,
`statx` on the file descriptor on Linux. The stamp is taken from the open file, never from the
path, so it describes the file whose bytes are hashed, and a link swapped in after the lookup
cannot point it at a file outside the share. Of the stamp:

- the modification time is shared with clients (it makes the tag);
- size, change time, creation time and inode are this node's own change check. The creation
  time is 0 where the file system or the system does not report it.

The device number is not part of the check: ZFS, btrfs and network file systems give a file
system another device number after a reboot, which would make every entry stale.

**Where.** The entry of `<path>` in share `<share>` is
`local/digests/<share>/<path>.splicefs_state`. Share names are safe folder names already.
Each name in `<path>` is escaped before it becomes a file or folder name:

1. `%` becomes `%25`.
2. Every byte that is not part of valid UTF-8 becomes `%XX` (upper case hex), so every name
   is valid UTF-8, which macOS requires.
3. In a name that ends in `.splicefs_state`, that last `.` becomes `%2E`.
4. A name whose escaped form plus `.splicefs_state` would be longer than 255 bytes becomes
   `%_` and the hex SHA-256 of the original name. `%_` cannot come from rules 1 to 3. The rule
   applies to every name in the path, folders too, with the length of the suffix counted
   even where no suffix follows.

Rule 3 keeps the two kinds of names apart: an entry always ends in `.splicefs_state` and a
folder never does, so a file `a` next to a folder `a.splicefs_state` gets two separate places.
Rules 1 to 3 can be undone, so two different paths never share an entry; rule 4 and file
systems that fold case or Unicode forms could still bring two paths to one file, which is why
the entry also holds the path (below).

`<path>` must be clean: names separated by single slashes, none empty, `.` or `..`, no slash
at the start or the end. Any other path is refused and has no entry, so no entry lands outside
the folder of its share and no file has two entries. Invalid UTF-8 is not a reason to refuse,
as rule 2 handles it.

The share root is a folder and has no digests, so it has no entry. A path that is not inside
the share, is hidden by the share's rules, or is too young for `min_age` gets `404` before the
cache is asked, so it never has an entry either. A symbolic link that is followed has the entry
of the link's path and the stamp of its target.

**What.** An entry is a small YAML file, mode 0600, in folders of mode 0700:

```yaml
version: 1
path: photos/2024/a.jpg      # the path in the share, escaped by rules 1 to 3
size: 52133
mtime_ns: 1763452800123456789
ctime_ns: 1763452801000000000
btime_ns: 0
inode: 81273
head: <64 hex digits>
tail: <64 hex digits>
```

`version` is a constant of the server, not a field a caller sets: the server writes it into
every entry and accepts only an entry that has it. It changes whenever what an entry means
changes, for example the 64 KiB of the digests; an entry of another version is stale. An entry
is accepted only in exactly the form the server writes, byte for byte, so a number with a
fraction or out of range, an unknown or missing field, a digest in upper case or a second YAML
document makes it invalid.

**Use.** A `HEAD` request resolves and opens the file and takes the stamp, the one stat of
the request: the size and the modification time it answers with come from the same stamp.
It then asks the in-memory cache and, if that misses, the entry. An entry is used only when `version`, `path`
and every stamp field (size, modification, change and creation time, inode) are equal to what
was just read; `path` is compared in its escaped form, as the entry stores it. Anything else is
a miss.

**Write.** On a miss the server reads the two 64 KiB ranges from the open file, takes the
stamp again, and stores the entry only if the two stamps are equal, so an entry never pairs a
stamp with bytes of a file that changed while it was read. The answer uses the digests either
way. The entry is written to a temporary file in `local/digests/.tmp/` and renamed over the old
one, so a reader sees the old entry or the new one, never part of one. A changed file gets a
new entry the same way the first time it is asked about after the change. Entries are not
flushed to disk: a lost entry costs one recompute. All access to `local/digests/` goes through
an `os.Root` on that folder, so a link placed inside it cannot send a write elsewhere.

**Failures.** An entry that is missing, unreadable, not a regular file, not valid YAML, of
another version, with a malformed field or in any form other than the one the server writes
is a miss: the digests are computed and the entry is
replaced. A failed write (full disk, no inodes left, read-only folder) never fails the request;
it is logged once as a warning, and again only after a write has succeeded in between. The
folder is the node's, so that holds for the node, not for each share: a full disk gives one
warning, whatever share the requests are for.

**Self-healing.** The digest store owns its folder: it makes `local/digests/` (mode 0700)
and its `.tmp/` when they are missing, at start and at every reload, and opens
`local/digests/` again when it is a new folder. A reload also makes the other state folders
that are missing, the same way a start does. So a digest folder removed while the server runs
is back, with its `.tmp/`, at the next reload. A reload leaves what is in `.tmp/`, since
requests may be writing there.

**Concurrency.** Two requests for one file may both miss and both write; each writes its own
temporary file and the last rename wins. Both hold the same digests for the same stamp, so no
lock is needed. Only this node's server writes `local/digests/`. The temporary folder is
emptied at start, before any share is opened, so nothing else is writing there then.

**Cleanup.** After start and after each reload the server sweeps the entries in the
background, one share at a time. For each entry it looks up the stored `path` through the
share, with the same rules as a request, and removes the entry when the file is gone, hidden or
its stamp differs. It removes folders left empty and the folders of shares that are no longer
configured. A renamed file therefore gets a new entry on its first request and its old entry
goes at the next sweep. A sweep can remove an entry written while it ran; that costs one
recompute, never a wrong answer. A sweep that fails on one entry logs it and goes on; an entry
whose file cannot be looked up (an error other than "not found") stays. Anything in a share's
folder that is not an entry the server wrote, at the place of the path it holds, is removed.
Only one sweep runs at a time: a reload ends the one that runs, and waits for it, before the
shares it uses are closed, and a stop ends it and waits for it too.

**Memory.** The in-memory cache stays in front of the entries and keeps its size (65536 per
share by default). It holds the same thing, keyed by the path and the same stamp fields with the
same comparison, so a hit there saves reading the entry.

**Cost.** Each entry takes one inode and, on most file systems, one block of the state file
system, about 4 KiB per data file asked about. The state folder must have room for that.

### Compression

A response is compressed only when the client asks for it, the request has no `Range`, and
the content type is compressible. Block requests never ask for compression, so they always
receive exact byte ranges.

### Configuration and state

The server reads two folders. Both may be missing or empty. A server with nothing configured
runs: it listens on all interfaces on port 8480 and serves TLS with a self-signed certificate
that it generates, unless a listener names a certificate or is `plain`. It has no shares and
no users until some are added.

The configuration folder holds what the administrator writes. `conf.d/` holds settings files,
merged in name order: maps merge key by key, and a list or value in a later file replaces the
earlier one. `shares.d/` holds one file per share, and the file name is the share name.
Packages ship both folders empty, so an upgrade never touches a file of the administrator.

The state folder holds what the server keeps:

```
<state>/
  local/                    this machine only, never synced
    node-id
    node-keys.yaml
    tls/cert.pem, key.pem
    digests/<share>/<path>.splicefs_state
  shared/                   may be synced between nodes
    users/<name>.yaml
    keys/<node-id>.yaml
    nodes/<node-id>.yaml
```

`local/` holds the node id, a UUIDv7 made on the first start, and the generated
certificate. `shared/` is safe to sync with any file sync tool because every file in it has
exactly one writer: the administrator writes each user file, and each server writes its own key
file and its own node file, named by its node id. Two servers never write the same file, so
there are no conflicts to resolve.

The node list is the union of the files in `shared/nodes/`. A server writes its own file when
`cluster.url` is set and removes it when it is not, so a server without a URL is not announced.
The cluster name comes from the configuration, not from a file.

The full reference is in the [server configuration](configuration.md).

### Node keys

Each node makes two key pairs on its first start and keeps the private halves in local/node-keys.yaml, which never
leaves the node. It publishes the public halves in its own node file in shared/nodes, so every node and every client
that lists the nodes learns them. The signing key will sign the node's tokens and the encryption key will receive
keys meant for this node only (later steps).

### Reloading

`SIGHUP` applies the configuration again without a restart. The server reads and checks
everything, loads certificates, opens new ports and opens shares first, and changes nothing if
any of that fails. New requests use the new configuration; requests in progress finish on the
one they started with. A listener that stays configured is never closed, so a new certificate
takes effect for new connections. Users, keys and nodes are followed as the files change and
need no reload. See "Applying changes" in the [server configuration](configuration.md).

### Logins

- One file per user in `shared/users/`: a password hash (argon2id, PHC string format), the
  shares the user may read, and an optional `disabled` flag.
- A token is a compact JWT signed with HMAC-SHA256. It carries the user name, the allowed
  shares and an expiry. Its header names the key that signed it. Any node can check a token
  whose key it has, with no communication between nodes.
- Each node signs with its own key and verifies against every key in `shared/keys/`. A node
  creates its file, `shared/keys/<node-id>.yaml`, on the first start and never replaces it.
  The file holds a list of keys, each with an id and a base64 secret of at least 32 bytes. The
  first key of a node's own file signs; every key of every file verifies. A token issued by
  one node is accepted by the others once the sync tool has delivered that node's file. Key
  ids must be unique across the folder: if two files use the same id, the file that sorts first
  keeps it and the clash is logged.
- Rotation is per node. On that node, add a new key with a new id as the first entry of its
  file, leave the old key below it for one token lifetime, then remove the old key. Nobody
  else edits that file.
- Tokens are short-lived (default 30 minutes). Removing or disabling a user stops new
  tokens at once; tokens already issued run out on their own.
- Expiry is checked with 30 seconds of leeway in both directions, so node clocks only need
  to agree roughly. Run NTP or an equivalent on every node.
- Login attempts are rate limited per client address. Behind a reverse proxy the address
  comes from `X-Forwarded-For`, and only when the request arrives from a configured trusted
  proxy.

The files under `shared/` are ordinary files. How they reach every node is the operator's
choice.

## Consistency

SpliceFS assumes: *a file with the same path, size and modification time has the same
content on every node.* It enforces what it can:

1. When a file is opened, the client sends `HEAD` to every node, groups the nodes by tag,
   head digest and tail digest, and reads only from the largest group. Ties go to the newest
   modification time.
2. Every block request carries `If-Match` with that tag. A node whose copy has changed
   answers `412`, and the client drops it for that file.
3. A file that disappears from every node during a read fails the read with an I/O error.

Beyond the head and tail digests, content hashes are not part of version 1. Copies that
differ only in the middle and agree in size and modification time are still taken for one
file. *Open:* an optional per-block checksum for deployments that cannot trust modification
times.

## Client

The client (`splicefs`) is one process that serves every configured mount. It is specified in
the [design document of the client repository](https://github.com/SpliceFS/client/blob/main/docs/design.md).
That document covers discovery, the merged tree, reading, the cache and filesystem behavior.

## Deployment shapes

- **Behind a reverse proxy.** The proxy terminates TLS and forwards the base path to the
  server over plain HTTP on a private address.
- **Standalone.** The server terminates TLS itself, with a certificate and key from its
  configuration or, when none is configured, with the certificate it generates for itself.
  Clients have to trust a generated certificate or be told to ignore it.
- **Several nodes.** Each node has its own configuration folder and its own shares, and the
  nodes sync `shared/` of their state folders. `local/` is never synced.

## Known limitations

- **Tokens cannot be revoked.** A removed or disabled user keeps access until the token
  already issued expires (30 minutes by default).
- **Keys travel by sync.** A token from one node is rejected by another until the sync tool
  has delivered the first node's key file.
- **Some changes need a restart.** A reload cannot change `log.format`, and it cannot move a
  listener to an overlapping address on the same port, for example from `0.0.0.0` to `::`,
  because the new address cannot be opened while the old one is open.

The limitations of the client are listed in its
[design document](https://github.com/SpliceFS/client/blob/main/docs/design.md#known-limitations).

## Open questions

- Per-block checksums (see Consistency).
- Revoking one token before it expires.
- Digest cache: whether entries are flushed to disk (proposed: no), how a failed write is
  logged (proposed: once until a write succeeds), and whether every `HEAD` opening the file to
  take the stamp from it costs too much (proposed: measure, keep).
