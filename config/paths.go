package config

import "strings"

// prefix starts every default path. It is empty for Linux packages and is set at build
// time (-ldflags -X github.com/SpliceFS/server/config.prefix=...) for systems whose
// package manager keeps its files under another root.
var prefix string

// DefaultConfigDir returns the configuration directory used when none is given.
func DefaultConfigDir() string { return underPrefix("/etc/splicefs/server") }

// DefaultStateDir returns the state directory used when none is given.
func DefaultStateDir() string { return underPrefix("/var/lib/splicefs/server") }

// underPrefix trims a trailing slash from the prefix so that it never yields "//".
func underPrefix(p string) string { return strings.TrimRight(prefix, "/") + p }
