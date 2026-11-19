//go:build linux

package filestat

import (
	"time"

	"golang.org/x/sys/unix"
)

// systemStat reads the Stamp of the file at path with one statx call. Flags 0 follow a
// symbolic link, as os.Stat does.
func systemStat(path string) (Stamp, error) {
	return statx(unix.AT_FDCWD, path, 0)
}

// systemFstat reads the Stamp of the open file fd with one statx call: an empty path with
// AT_EMPTY_PATH names the file the descriptor is open on.
func systemFstat(fd int) (Stamp, error) {
	return statx(fd, "", unix.AT_EMPTY_PATH)
}

// statx makes the statx call and takes the Stamp from its answer. Asking for the creation
// time costs nothing extra; the kernel says in the mask of the result whether it has one.
func statx(dirfd int, path string, flags int) (Stamp, error) {
	var stx unix.Statx_t
	call := func() error {
		return unix.Statx(dirfd, path, flags, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &stx)
	}
	if err := retryInterrupted(call); err != nil {
		return Stamp{}, err
	}
	return stampFromStatx(&stx), nil
}

// stampFromStatx takes the Stamp from the kernel's answer to statx. The device number is
// put together the way stat reports it, so a Stamp and an Identity of one file agree.
func stampFromStatx(stx *unix.Statx_t) Stamp {
	return Stamp{
		Size:  int64(stx.Size), //nolint:gosec // a file is never larger than 2^63 bytes
		Mtime: statxNanos(stx.Mtime),
		Ctime: statxNanos(stx.Ctime),
		Btime: birthNanos(stx),
		Dev:   unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		Ino:   stx.Ino,
		Mode:  fileMode(uint32(stx.Mode)),
	}
}

// birthNanos is the creation time of the file, or 0 when the file system does not record
// one: only a mask with STATX_BTIME in it says the field holds a time.
func birthNanos(stx *unix.Statx_t) int64 {
	if stx.Mask&unix.STATX_BTIME == 0 {
		return 0
	}
	return statxNanos(stx.Btime)
}

// statxNanos converts a statx time to nanoseconds since the Unix epoch.
func statxNanos(stamp unix.StatxTimestamp) int64 {
	return stamp.Sec*int64(time.Second) + int64(stamp.Nsec)
}
