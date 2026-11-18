SpliceFS server, the plain programs

This archive holds the programs and nothing else: no service file, no mount helper and no
install script. It is for a system the packages do not cover, or for people who want to place
every file themselves. The packages are the easier way where one exists for your system.

Put the programs where you want them, for example in a folder on your PATH. Then:

  splicefsd --version
  splicefsd --config DIR --state DIR           run the server
  splicefsd --config DIR config check          check the configuration files
  splicefsd --config DIR config show           print the configuration and where each value is from
  splicefsdctl hash-password                   make the password hash for a user file
  splicefsdctl --version

DIR after --config holds what you write: settings in conf.d/ and shares in shares.d/. DIR after
--state holds what the server keeps: users, keys and its certificate. Without the two options
the program uses the default folders of its system. A server with no configuration at all runs
with TLS on port 8480 and shares nothing.

Send the running server SIGHUP (kill -HUP PID) to apply a changed configuration without a
restart. Run it from the init system of your choice, as a user that can read the folders it
shares.

Files in the release are checked with SHA256SUMS and its signature SHA256SUMS.asc.

The real commands, every option and the format of the files: README.md and
docs/configuration.md at https://github.com/SpliceFS/server

The license is in the file LICENSE.
