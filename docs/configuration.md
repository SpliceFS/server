# Server configuration

The server reads a configuration directory, `/etc/splicefs/server` by default. `--config DIR`
overrides it.

The main file in the directory is `config.yaml`. Relative paths inside a configuration
file are relative to the configuration directory. Sizes take the suffixes `KiB`, `MiB`,
`GiB`, `TiB`; durations take `s`, `m`, `h`, `d`.

The format may change before the first release.

```yaml
# /etc/splicefs/server/config.yaml

# Where to listen. One entry per address and port.
listen:
  - address: 192.0.2.10     # the address of one interface; 0.0.0.0 or :: for all
    port: 8480
    # tls:                  # only when no reverse proxy terminates TLS
    #   cert_file: tls/fullchain.pem
    #   key_file: tls/privkey.pem

# URL prefix the API is served under.
base_path: /api

# Reverse proxies whose X-Forwarded-For header is believed.
trusted_proxies: [192.0.2.1]

# Directories to share. The key is the share's name and its top-level folder for readers.
shares:
  media:
    path: /srv/media
    exclude: [".*"]         # glob patterns for names never listed or served
    min_age: 0s             # ignore files modified more recently than this
    follow_symlinks: false  # links that stay inside the share may be followed when true

cluster:
  name: example
  node: east                          # this node's name in the node list
  nodes_file: /srv/shared/nodes.yaml  # the same file on every node

auth:
  users_dir: /srv/shared/users        # one <user>.yaml per user
  keys_file: /srv/shared/keys.yaml    # token signing keys, mode 600
  token_ttl: 30m                      # checked with 30s of leeway; keep node clocks in step with NTP
  login_rate: 10/m                    # attempts per client address

mtime_precision: 1s   # modification times are compared at this precision

log:
  level: info         # debug, info, warn, error
  format: text        # text or json
```

## Node list

```yaml
# nodes.yaml
cluster: example
nodes:
  - name: east
    url: https://east.example.com/api
  - name: west
    url: https://west.example.com/api
```

## User file

```yaml
# users/alice.yaml
password_hash: "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>"
shares: [media]     # ["*"] allows every share
disabled: false
```

`splicefsd hash-password` reads a password from standard input and prints the hash.

## Keys file

```yaml
# keys.yaml: the first key signs, every key verifies
keys:
  - id: 2026-11
    secret: "<base64, at least 32 random bytes>"
```

The configuration of the client is described in the
[client configuration reference](https://github.com/SpliceFS/client/blob/main/docs/configuration.md).
