//go:build unix

package filestat

import (
	"io/fs"
	"syscall"
)

// Stamp is everything the file system reports that tells one version of a data file from
// another, read together so no part of it can come from a different moment than the rest.
// Times are in nanoseconds since the Unix epoch.
type Stamp struct {
	Size int64
	// Mtime is the modification time, which a program can set to any value.
	Mtime int64
	// Ctime is the change time, which the kernel sets and a program cannot put back.
	Ctime int64
	// Btime is the creation (birth) time. It is 0 where the file system or the operating
	// system does not report it.
	Btime int64
	// Dev and Ino say which file this is; see Identity.
	Dev  uint64
	Ino  uint64
	Mode fs.FileMode
}

// fileMode converts the mode bits of a stat result to the type and permission bits Go uses,
// the way os.Stat does, so a Stamp agrees with the FileInfo of the same file.
func fileMode(raw uint32) fs.FileMode {
	mode := fs.FileMode(raw&0o777) | typeMode(raw)
	if raw&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if raw&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if raw&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// typeMode returns the file type bits of a stat result's mode. A regular file has none.
func typeMode(raw uint32) fs.FileMode {
	switch raw & syscall.S_IFMT {
	case syscall.S_IFBLK:
		return fs.ModeDevice
	case syscall.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFDIR:
		return fs.ModeDir
	case syscall.S_IFIFO:
		return fs.ModeNamedPipe
	case syscall.S_IFLNK:
		return fs.ModeSymlink
	case syscall.S_IFSOCK:
		return fs.ModeSocket
	default:
		return 0
	}
}
