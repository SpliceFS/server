//go:build darwin || freebsd

package share

import "syscall"

// setChangeTime sets the change time of stat to the given number of nanoseconds.
func setChangeTime(stat *syscall.Stat_t, nanoseconds int64) {
	stat.Ctimespec = syscall.NsecToTimespec(nanoseconds)
}
