//go:build darwin

package config

// macOS has no package manager that owns /etc, so a program run by a user keeps its files
// in that user's home directory.
const (
	osConfigDir = "~/.splicefs/server"
	osStateDir  = "~/.splicefs/var/server"
)
