# Identity, keys and encryption (proposed)

Status: proposed, for review. When a part is built, its text moves into
[design.md](design.md) and the [server configuration](configuration.md), and is removed here.

## What changes and why

- Logins move from passwords to key pairs. A user proves who they are by holding a private
  key; the cluster stores only the public key.
- Everything has a UUIDv7 as its identifier: the cluster, each node, each user, each key.
  Names (user name, node name, cluster name) stay as readable labels and are never used to
  find a file or to identify anything in the protocol.
- Two levels of authentication, separate from TLS: the cluster level (login, discovery) and
  the node level (every other route). TLS stays underneath as transport only.
- State files are written once and never edited where that matters (keys), and are read
  again on use where it is cheap (users at login). No decision depends on telling an old
  version of an edited file from a new one by its timestamp.
- The head and tail digests of data files are kept in state, per node, so a restart does not
  recompute them.

## Three kinds of files

| Kind | What | Where | Written by |
|---|---|---|---|
| data | the shared files | the shares | the operator's tools; the server never writes a share |
| config | settings for the whole cluster | the configuration folder | the administrator |
| state | users, nodes, keys, the cluster identity, digest caches | the state folder | the server and its admin commands |

All three are synced between nodes outside SpliceFS, by the operator's choice of tool. Only
data is served through the API.

## Identities and keys

Every identity (cluster, node, user) has:

- an id: a UUIDv7;
- a readable name;
- a signing key pair, Ed25519: proves identity by signature;
- an encryption key pair, X25519: lets others encrypt to it.

Ed25519 keys cannot encrypt, which is why there are two pairs. Both come from Go's standard
library (`crypto/ed25519`, `crypto/ecdh`), as do HKDF-SHA256 and AES-256-GCM, used below.

Every public key has its own id, a UUIDv7, and lives in its own file that is written once and
never edited. Rotating a key means writing a new key file and later removing the old one. So
the server only ever has to notice a key file appear or disappear, which a folder listing
shows by name, with no timestamp involved.

| Identity | Created by | Private keys | Public keys |
|---|---|---|---|
| cluster | `splicefsd cluster init`, run once on one node | on every node (the operator copies them) | every node, every client package |
| node | the server, on its first start | that node only | every node; the node inventory |
| user | the client, `splicefs key generate` | that user's machine only | the cluster (`splicefsd user add`) |

The cluster's private keys are on every node, because every node answers cluster-level
routes. Anyone who has a node's state folder can act as the cluster.

## State layout

```
<state>/
  local/                           this node only, never synced
    node.yaml                      this node's id and private keys (mode 0600)
    tls/cert.pem, key.pem          the generated TLS certificate, as today
    digests/<share>/<path>.splicefs_state
  shared/                          synced between nodes
    cluster/cluster.yaml           cluster id, name, public keys
    cluster/private.yaml           cluster private keys (mode 0600)
    nodes/<node-id>.yaml           node name, URL, ids of its public keys
    users/<user-id>.yaml           user name, ids of its public keys, shares, disabled
    keys/<key-id>.yaml             one public key: owner id, kind (sign or encrypt), key
```

- Every file in `shared/` still has exactly one writer, so syncing never conflicts: the
  server writes its node file and its key files; admin commands write cluster and user files.
- State files are written to a temporary name and renamed into place, as today. A rename
  gives the file a new inode.
- User files are named by id. `splicefsd user add`, `user disable`, `user rename` and the like
  find a user by name or id. Two users may not share a name.
- The digest caches are in `local/`, because the stamp they hold (device, inode) is only
  meaningful on this node. A node's id in the path is not needed there; `local/` is already
  per node.

## Login (cluster level)

1. The client sends `POST /v2/login/challenge` with the user's name or id, encrypted to the
   cluster's encryption key (see Encryption).
2. The server reads the user's file (every login). For an unknown or disabled user it answers
   exactly as for a known one, so the answer does not reveal which users exist.
3. The server makes a random 32-byte value, encrypts it to the user's encryption key, and
   returns it with a challenge record: user id, a hash of the value, an expiry 60 seconds
   ahead. The record is signed with the cluster's signing key, so any node can check it and no
   node has to remember it.
4. The client checks the cluster's signature, decrypts the value and sends it back with the
   record, `POST /v2/login`.
5. The server checks the record's signature and expiry, compares the hash, reads the user's
   file again, and issues a token.

A value can be answered more than once within its 60 seconds. That only gives the same user
another token; it needs the user's private key to answer at all.

## Tokens

- A JWT signed with EdDSA by the issuing node's signing key. Its header names the key id.
- Claims: user id, issuing node id, issued at, expiry. No shares: those are read from the
  user's state (below), so a change applies without a new token.
- Lifetime 5 minutes. `POST /v2/token/renew` with a valid token reads the user's file in full
  and issues a new token. That costs a few milliseconds every 5 minutes per mount.
- Every node verifies a token with the issuing node's public key from `shared/keys/`. The keys
  are kept in memory; a key id the node does not know makes it list `shared/keys/` again.
- Expiry has 30 seconds of leeway in both directions, as today.

## User state on every request

A block request must not read files beyond the data it serves. So:

- The node keeps each user's state (shares, disabled) in memory.
- On a request, if this user's file was last checked more than 30 seconds ago, the node runs
  one `stat` on it. If its inode, size, modification time or change time differ, it reads the
  file again.
- So a disabled user, or a removed share, loses access within 30 seconds. A renewal reads the
  file in full in any case, which bounds even a missed in-place edit to one token lifetime.

## Encryption (application level)

Every request and response body on a `/v2` route is encrypted, in addition to TLS.

- Node routes: the key is derived from X25519 between the user's encryption key and the
  node's encryption key, HKDF-SHA256 with both ids as context. The client and the node each
  compute it alone; no extra round trip.
- Cluster routes: the same with the cluster's encryption key. The first login message, before
  the user is known to be who they say, uses an ephemeral key of the client instead of the
  user's key.
- Each body is sealed with AES-256-GCM, a random nonce per message, and the method, path,
  token and a timestamp as associated data, so a body cannot be moved to another request. A
  node refuses a timestamp more than 30 seconds off.
- A message that decrypts also proves it came from the holder of the user's private key, on
  every request, not only at login.
- These keys are static, so they give no forward secrecy on their own. TLS underneath does.
- Cost: AES-GCM with hardware support runs at several GB/s per core. The block path will be
  measured before and after; if it costs too much, that is reported, not worked around.

## Data digests

- The cache file for `<share>/<path>` is `local/digests/<share>/<path>.splicefs_state`. It
  holds the stamp and the head and tail digests.
- The stamp: name, size, modification time, change time, creation time (0 where the file
  system does not record it), device and inode. All of it comes from one system call: `stat`
  on macOS and FreeBSD, `statx` on Linux. The check costs the same for any file size.
- A stamp that differs in any field means the digests are computed again and the file
  rewritten. The in-memory cache stays in front of it.

## Discovery

`GET /v2/nodes` (cluster level) lists each node's id, name, URL and the ids and values of its
public keys. A client checks each node's answers against those keys.

## Client package

`splicefsd user add` prints a package for the user: the cluster id, name and public keys, the
URL of at least one node, and the user's id. The client keeps its own private keys next to
it. The package holds no secret.

## Compatibility

Removing password login breaks every older client, so this ships as a new minor version of
both server and client, released together, with the API moved to `/v2`. `/v1` is removed in
the same release.

## Order of work

Each step is one pull request with its own tests, small enough to review.

1. Server: node id as a UUIDv7 and node key pairs in `local/node.yaml`; public key files.
   No protocol change.
2. Server: digest cache files in `local/digests/`, with the full stamp from one system call.
3. Server: `splicefsd cluster init`; cluster files.
4. Server: users by id, with name as a label; `user add` takes the user's public keys and
   prints the package; user state check (30 seconds, one `stat`).
5. Server: EdDSA tokens of 5 minutes, renewal; HMAC key files removed.
6. Server: challenge login on `/v2`; password login removed.
7. Server: body encryption on `/v2`; measure the block path.
8. Client: `splicefs key generate`, package import, challenge login, renewal, encryption,
   node key checks.

## For review

- The cluster's private keys are on every node and travel with the state sync (decided).
- The digest caches are in `local/digests/`, not `shared/nodes/<node-id>/`: they mean nothing
  on another node, so syncing them would only copy files around.
- Shares move out of the token into user state, so a share change applies within 30 seconds.
- `/v1` is removed in the same release instead of kept alongside `/v2`.
