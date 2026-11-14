//go:build unix

// Package filestat reads from the file system what tells one version of a file from
// another when its size and modification time cannot: a program can set a modification
// time to any value, and a coarse clock gives two writes the same one.
package filestat

import (
	"io/fs"
	"syscall"
)

// Identity says which file a FileInfo describes and when that file last changed in any
// way. The device and inode number differ for every file that exists at the same time, so a
// file renamed over another has another identity even when size and modification time are
// equal. The change time (ctime) is set by the kernel to the current time whenever a file
// is written, renamed or has its times set, so a program cannot put it back.
type Identity struct {
	Dev uint64
	Ino uint64
	// Ctime is in nanoseconds since the Unix epoch.
	Ctime int64
}

// Of returns the identity of the file that info describes, taken from the file system's own
// description of it. It is not ok for a FileInfo that does not come from the file system,
// which carries no device, inode or change time.
func Of(info fs.FileInfo) (Identity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return Identity{Dev: asUint64(stat.Dev), Ino: asUint64(stat.Ino), Ctime: changeTime(stat)}, true
}

// asUint64 keeps the bits of a number the kernel reported. The types of the device and
// inode number differ between systems, and macOS reports a device number that can be
// negative; two different numbers must stay different, so no value is clamped.
func asUint64[T ~int32 | ~uint32 | ~int64 | ~uint64](value T) uint64 {
	return uint64(value)
}
