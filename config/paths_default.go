//go:build !darwin

package config

// Directories of a system-wide installation, which a package manager manages.
const (
	osConfigDir = "/etc/splicefs/server"
	osStateDir  = "/var/lib/splicefs/server"
)
