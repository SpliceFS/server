//go:build darwin || freebsd

package filestat

import "syscall"

// systemStat reads the Stamp of the file at path with one stat call, which follows a
// symbolic link as os.Stat does. macOS and FreeBSD report the birth time with every stat.
func systemStat(path string) (Stamp, error) {
	var stat syscall.Stat_t
	if err := retryInterrupted(func() error { return syscall.Stat(path, &stat) }); err != nil {
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
		Btime: stat.Birthtimespec.Nano(),
		Dev:   asUint64(stat.Dev),
		Ino:   asUint64(stat.Ino),
		Mode:  fileMode(uint32(stat.Mode)),
	}
}
