//go:build unix

package filestat

import (
	"io/fs"
	"syscall"
	"testing"
)

func TestFileModeMatchesWhatOsStatReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  uint32
		want fs.FileMode
	}{
		{"regular file", syscall.S_IFREG | 0o644, 0o644},
		{"directory", syscall.S_IFDIR | 0o755, fs.ModeDir | 0o755},
		{"symbolic link", syscall.S_IFLNK | 0o777, fs.ModeSymlink | 0o777},
		{"named pipe", syscall.S_IFIFO | 0o600, fs.ModeNamedPipe | 0o600},
		{"socket", syscall.S_IFSOCK | 0o600, fs.ModeSocket | 0o600},
		{"block device", syscall.S_IFBLK | 0o660, fs.ModeDevice | 0o660},
		{"character device", syscall.S_IFCHR | 0o666, fs.ModeDevice | fs.ModeCharDevice | 0o666},
		{"set user id", syscall.S_IFREG | syscall.S_ISUID | 0o755, fs.ModeSetuid | 0o755},
		{"set group id", syscall.S_IFREG | syscall.S_ISGID | 0o755, fs.ModeSetgid | 0o755},
		{"sticky", syscall.S_IFDIR | syscall.S_ISVTX | 0o777, fs.ModeDir | fs.ModeSticky | 0o777},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fileMode(tt.raw); got != tt.want {
				t.Errorf("fileMode(%#o) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
