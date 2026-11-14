//go:build unix

package share

import (
	"os"
	"syscall"
)

// newDigestKey builds the cache key for the file at relPath from the file system's own
// description of it. A FileInfo that does not come from the file system carries no device,
// inode or change time, and its key is not identified, so nothing is cached for it.
func newDigestKey(relPath string, fileInfo os.FileInfo) digestKey {
	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return digestKey{}
	}
	return digestKey{
		path:       relPath,
		dev:        asUint64(stat.Dev),
		ino:        asUint64(stat.Ino),
		size:       fileInfo.Size(),
		mtime:      fileInfo.ModTime().UnixNano(),
		ctime:      changeTime(stat),
		identified: true,
	}
}

// asUint64 keeps the bits of a number the kernel reported. The types of the device and
// inode number differ between systems, and macOS reports a device number that can be
// negative; two different numbers must stay different, so no value is clamped.
func asUint64[T ~int32 | ~uint32 | ~int64 | ~uint64](value T) uint64 {
	return uint64(value)
}
