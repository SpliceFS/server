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
| `GET /v1/nodes` | `{"cluster":"...","nodes":[{"name":"...","url":"..."}]}` from the files in `shared/nodes/`. |
| `GET /v1/tree/` | The shares this user may read, as directory entries. |
| `GET /v1/tree/<share>/<dir>/` | `{"entries":[{"name":"...","dir":false,"size":123,"mtime":"<RFC 3339>","etag":"..."}]}`, sorted by name. `404` if the directory does not exist on this node. |
| `GET`, `HEAD /v1/file/<share>/<path>` | The file. Supports `Range` (one range), `If-Range` and `If-Match`. Sends `ETag` and `Accept-Ranges: bytes`. `HEAD` also sends `X-SpliceFS-Head-Digest` (see File identity). |
| `GET /v1/space/<share>` | `{"total":<bytes>,"free":<bytes>}` for the filesystem holding the share. |

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

A `HEAD` request also returns `X-SpliceFS-Head-Digest`: the SHA-256 of the first 64 KiB of
the file. It costs one small read, which the server remembers per file until the file's size
or modification time changes. It catches two different files that happen to share a size
and a modification time.

Two copies are treated as the same file when their tag and their head digest are equal.

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
    tls/cert.pem, key.pem
  shared/                   may be synced between nodes
    users/<name>.yaml
    keys/<node-id>.yaml
    nodes/<node-id>.yaml
```

`local/` holds the node id, a random value made on the first start, and the generated
certificate. `shared/` is safe to sync with any file sync tool because every file in it has
exactly one writer: the administrator writes each user file, and each server writes its own key
file and its own node file, named by its node id. Two servers never write the same file, so
there are no conflicts to resolve.

The node list is the union of the files in `shared/nodes/`. A server writes its own file when
`cluster.url` is set and removes it when it is not, so a server without a URL is not announced.
The cluster name comes from the configuration, not from a file.

The full reference is in the [server configuration](configuration.md).

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

1. When a file is opened, the client sends `HEAD` to every node, groups the nodes by tag and
   head digest, and reads only from the largest group. Ties go to the newest modification time.
2. Every block request carries `If-Match` with that tag. A node whose copy has changed
   answers `412`, and the client drops it for that file.
3. A file that disappears from every node during a read fails the read with an I/O error.

Beyond the head digest, content hashes are not part of version 1. *Open:* an optional
per-block checksum for deployments that cannot trust modification times.

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
