//go:build unix && !darwin && !freebsd

package share

import "syscall"

// changeTime returns the change time (ctime) of stat in nanoseconds. Linux names the field
// Ctim; macOS and FreeBSD name it Ctimespec.
func changeTime(stat *syscall.Stat_t) int64 {
	return stat.Ctim.Nano()
}
