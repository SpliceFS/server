//go:build darwin || freebsd

package share

import "syscall"

// changeTime returns the change time (ctime) of stat in nanoseconds. macOS and FreeBSD name
// the field Ctimespec; Linux names it Ctim.
func changeTime(stat *syscall.Stat_t) int64 {
	return stat.Ctimespec.Nano()
}
