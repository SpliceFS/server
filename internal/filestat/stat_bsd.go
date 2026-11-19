//go:build darwin || freebsd

package filestat

import "syscall"

// systemStat reads the Stamp of the file at path with one stat call, which follows a
// symbolic link as os.Stat does.
func systemStat(path string) (Stamp, error) {
	return stampFrom(func(stat *syscall.Stat_t) error { return syscall.Stat(path, stat) })
}

// systemFstat reads the Stamp of the open file fd with one fstat call.
func systemFstat(fd int) (Stamp, error) {
	return stampFrom(func(stat *syscall.Stat_t) error { return syscall.Fstat(fd, stat) })
}

// stampFrom runs one of the stat calls and takes the Stamp from its answer. macOS and
// FreeBSD report the birth time with every stat call.
func stampFrom(call func(*syscall.Stat_t) error) (Stamp, error) {
	var stat syscall.Stat_t
	if err := retryInterrupted(func() error { return call(&stat) }); err != nil {
		return Stamp{}, err
	}
	return stampFromStat(&stat), nil
}

// stampFromStat takes the Stamp from the file system's own description of a file, with the
// same times and numbers Of reads.
func stampFromStat(stat *syscall.Stat_t) Stamp {
	return Stamp{
		Size:  stat.Size,
		Mtime: stat.Mtimespec.Nano(),
		Ctime: changeTime(stat),
		Btime: birthTime(stat.Birthtimespec),
		Dev:   asUint64(stat.Dev),
		Ino:   asUint64(stat.Ino),
		Mode:  fileMode(uint32(stat.Mode)),
	}
}

// birthTime is the creation time in nanoseconds, or 0 when the file system does not record
// one: FreeBSD then reports -1 seconds.
func birthTime(birth syscall.Timespec) int64 {
	if birth.Sec < 0 {
		return 0
	}
	return birth.Nano()
}
