# Server configuration

Nothing has to be configured. A fresh install runs with no shares and no users. It listens on
every interface on port 8480 with TLS, using a certificate that it creates for itself. You
add settings, shares and users only when you need them.

The server uses two folders:

| Folder        | Linux packages            | macOS build              | Holds                       |
|---------------|---------------------------|--------------------------|-----------------------------|
| Configuration | `/etc/splicefs/server`    | `~/.splicefs/server`     | what you write: settings, shares |
| State         | `/var/lib/splicefs/server`| `~/.splicefs/var/server` | what the server keeps: users, keys, certificate |

`--config DIR` and `--state DIR` override them:

```
splicefsd --config /srv/splicefs/etc --state /srv/splicefs/var
```

Upgrades never touch the files you put in these folders. A missing folder is the same as an
empty one.

The format may change before the first release.

## Settings

Settings are files in `conf.d/` inside the configuration folder.

- Only files whose name ends in `.yaml` are read. Names that start with a dot, folders and all
  other files are ignored, so editor backups and temporary files of sync tools do no harm.
- Files are read in name order (byte order, so `10-a.yaml` comes before `20-b.yaml`). A later
  file wins over an earlier one.
- Maps merge key by key. A list or a single value in a later file replaces the earlier one
  whole. For example, a `listen` list in a later file replaces the entire earlier `listen` list.
- Each file is checked on its own. An unknown key, or more than one YAML document in a file,
  is an error that names the file.
- After merging, the result is validated and every problem is reported together. A problem
  names the file its value came from.
- Relative file paths in a settings file (the TLS files) are relative to the configuration
  folder.
- Durations take the suffixes `s`, `m`, `h` and `d` (one day is 24 hours).

This file sets every key to its default. You do not need to write any of it.

```yaml
# /etc/splicefs/server/conf.d/00-example.yaml

listen:
  - address: "::"           # "::" is every interface; 0.0.0.0 is every IPv4 interface
    port: 8480
    # tls:                  # your own certificate; both files or neither
    #   cert_file: tls/fullchain.pem
    #   key_file: tls/privkey.pem
    plain: false            # true: no TLS, for use behind a reverse proxy

base_path: /api             # URL prefix the API is served under
trusted_proxies: []         # reverse proxies whose X-Forwarded-For header is believed

cluster:
  name: default
  # node: east              # default: the host name in lower case
  url: ""                   # empty: this server is not announced to clients

auth:
  token_ttl: 30m
  login_rate: 10/m          # login attempts per client address

mtime_precision: 1s         # modification times are compared at this precision

log:
  level: info               # debug, info, warn or error
  format: text              # text or json
```

| Key                 | Meaning                                  | Default           | Limits                  |
|---------------------|------------------------------------------|-------------------|-------------------------|
| `listen`            | List of listeners                        | one, see below    | at least one entry      |
| `listen[].address`  | IP address to bind                       | `::`              | an IPv4 or IPv6 address, not a host name |
| `listen[].port`     | TCP port                                 | `8480`            | 1 to 65535              |
| `listen[].tls`      | Your own certificate for this listener   | none              | `cert_file` and `key_file` both or neither |
| `listen[].plain`    | Serve without TLS                        | `false`           | must not be `true` together with `tls` |
| `base_path`         | URL prefix of the API                    | `/api`            | must start with `/`; a trailing `/` is dropped; `/` serves at the root |
| `trusted_proxies`   | Proxies whose `X-Forwarded-For` is believed | empty          | addresses or CIDR ranges |
| `cluster.name`      | Name of the cluster this server is in    | `default`         | none                    |
| `cluster.node`      | Name of this server in the node list     | host name, lower case | lower-case letters, digits, `.`, `_`, `-`; starts with a letter or digit; at most 64 characters |
| `cluster.url`       | Address clients use to reach this server | empty             | absolute `http` or `https` URL with a host, no `?` or `#`; a trailing `/` is dropped |
| `auth.token_ttl`    | How long a login token is valid          | `30m`             | at least `1m`, not `unlimited` |
| `auth.login_rate`   | Login attempts allowed per client address | `10/m`           | `<count>/<s\|m\|h>`, count at least 1 |
| `mtime_precision`   | Precision for comparing modification times | `1s`            | at least `1s`, not `unlimited` |
| `log.level`         | Log detail                               | `info`            | `debug`, `info`, `warn`, `error` |
| `log.format`        | Log format                               | `text`            | `text`, `json`          |

Notes on the table:

- `token_ttl` is checked with 30 seconds of leeway. Keep the clocks of all servers in step
  with NTP.
- `cluster.url` should be the address clients type, including the base path, for example
  `https://east.example.com:8480/api`. See "Data" for what the server does with it.
- Two entries in `listen` must not have the same address and port.

### Listeners and TLS

A listener serves HTTPS. Which certificate it uses depends on its keys:

- Neither `tls` nor `plain`: the server uses the certificate it created for itself (see
  "Data"). Clients have to trust it or be told to ignore it. This is the default.
- `tls` with `cert_file` and `key_file`: the listener uses your certificate. Put the full
  chain in `cert_file`.
- `plain: true`: no TLS at all. Use this only behind a reverse proxy that terminates TLS, and
  list that proxy in `trusted_proxies`.

`plain: true` together with `tls` is an error.

```yaml
# conf.d/10-listen.yaml: own certificate on port 443
listen:
  - address: "::"
    port: 443
    tls:
      cert_file: tls/fullchain.pem
      key_file: tls/privkey.pem
```

```yaml
# conf.d/10-listen.yaml: behind a proxy on the same machine
listen:
  - address: 127.0.0.1
    port: 8480
    plain: true
trusted_proxies: [127.0.0.1]
```

## Shares

A share is a folder that the server makes available. Each share is one file in `shares.d/`
inside the configuration folder. The file name without `.yaml` is the share name, and it is
the top-level folder that readers see. The same rules as for `conf.d/` apply to which files
are read. No shares is valid.

A share name has lower-case letters, digits, `.`, `_` and `-`, starts with a letter or digit,
and has at most 64 characters.

| Key               | Meaning                                         | Default   | Limits                |
|-------------------|-------------------------------------------------|-----------|-----------------------|
| `path`            | Folder to share                                 | required  | absolute path         |
| `exclude`         | Glob patterns for names that are never listed or served | `[".*"]` | each pattern matches one name, so no `/` |
| `min_age`         | Ignore files modified more recently than this   | `0s`      | finite, at least `0`  |
| `follow_symlinks` | Follow links that stay inside the share         | `false`   | `true` or `false`     |

Setting `exclude` replaces the default list, so write `".*"` again if you still want names
that start with a dot hidden. `exclude: []` hides nothing.

```yaml
# /etc/splicefs/server/shares.d/media.yaml
path: /srv/media
exclude: [".*", "*.tmp"]
min_age: 30s
follow_symlinks: false
```

`splicefsd config check` does not look at the folder. If `path` cannot be opened, the server
does not start, or does not apply the change when it reloads, and says which share.

## Data

The state folder holds what the server creates and what you add to it. The server makes the
folders it needs. It must be able to write to the state folder.

```
<state>/
  local/                    this machine only
    node-id
    tls/cert.pem
    tls/key.pem
  shared/                   may be synced between servers
    users/<name>.yaml
    keys/<node-id>.yaml
    nodes/<node-id>.yaml
```

| File                         | Written by            | Content                                 |
|------------------------------|-----------------------|-----------------------------------------|
| `local/node-id`              | the server, once      | 16 lower-case hex characters            |
| `local/tls/cert.pem`, `key.pem` | the server, once   | the certificate the server made for itself |
| `shared/users/<name>.yaml`   | you                   | one user                                |
| `shared/keys/<node-id>.yaml` | that server only      | token signing keys                      |
| `shared/nodes/<node-id>.yaml`| that server only      | name and address of that server         |

### local/

Never copy or sync `local/`. It belongs to this machine.

- `node-id` is made on the first start and never changes. It names this server's files in
  `shared/`. Do not edit it. If you delete it, the server makes a new one, and the files of
  the old id stay in `shared/` until you remove them.
- `tls/` holds a self-signed certificate (ECDSA P-256, valid for 10 years) made on the first
  start for the host name, `localhost`, `127.0.0.1` and `::1`. Files that exist are never
  replaced. To use your own certificate, set `tls` on the listener instead.

### shared/

You may sync `shared/` between servers with any file sync tool. Sync this one folder: `shared/`,
and nothing else. Every file in it has exactly one writer, so servers never overwrite each
other.

**users/** holds one file per user. You write these files. The file name without `.yaml` is
the user name, with the same rules as a share name.

```yaml
# <state>/shared/users/alice.yaml
password_hash: "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>"
shares: [media]     # share names; ["*"] allows every share
disabled: false     # true: the user cannot log in
```

To add a user:

1. Run `splicefsd hash-password`. It reads one line from standard input, the password, and
   prints the hash. Typed input is shown on the screen; to avoid that, pipe the password in
   from a file.
2. Create `<state>/shared/users/<name>.yaml` with the file content above. Paste the printed
   hash as `password_hash` and list the shares the user may use. Only `password_hash` is
   required; a user without `shares` can log in but cannot use any share.
3. Make sure the user the server runs as can read the file.

No restart is needed. The file is read at each login, so a new, changed or removed file takes
effect at once. If a user's file cannot be parsed, logins for that user fail until you fix it.

**keys/** holds the keys that sign and verify login tokens. The server makes its own file on
the first start with one key, named `<node-id>-1`, with a random secret. You do not write
these files.

```yaml
# <state>/shared/keys/<node-id>.yaml
keys:
  - id: "<node-id>-1"
    secret: "<base64, at least 32 random bytes>"
```

The first key in a server's own file signs. Every key in every file of the folder verifies.
When the sync tool has delivered another server's file, a token from that server is accepted
here. An existing file is never replaced.

**nodes/** holds the node list that clients ask for. When `cluster.url` is set, the server
writes `nodes/<node-id>.yaml` with its `cluster.node` name and its `cluster.url`. When
`cluster.url` is empty, the server removes that file and is not announced.

```yaml
# <state>/shared/nodes/<node-id>.yaml
name: east
url: https://east.example.com:8480/api
```

The node list that a server gives to clients is every file in `nodes/`, so it is empty when
no server has a `cluster.url`. The cluster name comes from `cluster.name` in the settings.

Changes in `users/`, `keys/` and `nodes/` are picked up without a restart. A keys or nodes
file that cannot be parsed is ignored with a warning in the log, and the last good content of
that file stays in use. Changes to `conf.d/` and `shares.d/` take effect when you reload the
server, see "Applying changes".

## Applying changes

The server applies changes to `conf.d/` and `shares.d/` without a restart. Send it `SIGHUP`:

```
systemctl reload splicefsd        # Linux packages with systemd
rc-service splicefsd reload       # Alpine
kill -HUP <pid>                   # by hand
```

Run `splicefsd config check` first. It finds mistakes in the files before the server reads them.

- The server reads and checks the whole configuration, loads the certificates, opens the new
  ports and opens the shares before it changes anything. If any of that fails, nothing
  changes. The server keeps running on the configuration it had, and the log has a line
  `configuration not applied` with the reason. On success the log has `configuration applied`.
- New requests use the new configuration. Requests in progress finish on the configuration
  they started with, so a download from a share that was just removed completes.
- A listener whose address and port stay configured is never closed. A new certificate, or a
  switch between TLS and `plain`, takes effect for new connections.
- A removed listener stops accepting connections. Its open connections finish. An added
  listener starts accepting.
- `log.level` follows the configuration. `log.format` is read only at start.
- The node file in `shared/nodes/` follows `cluster.node` and `cluster.url`.
- Users, keys and nodes under the state folder never need a reload. They are picked up as
  they change.

A restart is needed in two cases. One is a change of `log.format`. The other is moving a
listener to an overlapping address on the same port, for example from `0.0.0.0` to `::`. The
new address cannot be opened while the old one is open, so the reload is refused with
`configuration not applied`.

## Checking

Check the configuration before you start the server or reload it:

```
splicefsd config check
```

This prints `configuration ok` and exits with status 0. If something is wrong, it prints every
problem, names the file each came from, and exits with status 1. It does not read certificates
or the state folder, and it does not open the share folders. The server reports those problems
when it starts or reloads.

```
splicefsd config show
```

This prints the merged settings as YAML, then the shares. A value that a file set ends with a
comment naming that file. A value without a comment is a default. A share shows only what its
file changed.

```
listen: # 10-listen.yaml
  - address: '::'
    port: 8480
    plain: false
base_path: /api
cluster:
  name: example # 10-listen.yaml
  node: east # 20-local.yaml
  url: https://east.example.com:8480/api # 10-listen.yaml
auth:
  token_ttl: 1h # 20-local.yaml
  login_rate: 10/m
mtime_precision: 1s
log:
  level: debug # 20-local.yaml
  format: text
trusted_proxies: []
shares:
  media:
    path: /srv/media # media.yaml
    min_age: 30s # media.yaml
```

Both commands take `--config DIR`.

## Several servers

To run several servers as one cluster:

1. Give every server the same `cluster.name`.
2. On each server, set its own `cluster.url`, and a different `cluster.node` if the host
   names are not what you want clients to see.
3. Sync `<state>/shared/` between all servers with any file sync tool.
4. Never sync `local/`. Each server keeps its own.
5. Keep the clocks in step with NTP.

Each server keeps its own `conf.d/` and `shares.d/`. Users are written once and reach the
other servers through the sync of `shared/`. Each server writes only its own `keys/` and
`nodes/` files.

The configuration of the client is described in the
[client configuration reference](https://github.com/SpliceFS/client/blob/main/docs/configuration.md).
