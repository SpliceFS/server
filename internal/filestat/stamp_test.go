//go:build unix

package filestat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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

func TestRetryInterruptedCallsAgainUntilItIsNotInterrupted(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryInterrupted(func() error {
		calls++
		if calls < 3 {
			return syscall.EINTR
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("retryInterrupted = %v after %d calls, want nil after 3", err, calls)
	}
}

func TestRetryInterruptedReturnsAnyOtherErrorAtOnce(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryInterrupted(func() error {
		calls++
		return syscall.ENOENT
	})
	if !errors.Is(err, syscall.ENOENT) || calls != 1 {
		t.Errorf("retryInterrupted = %v after %d calls, want ENOENT after 1", err, calls)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustStat(t *testing.T, path string) Stamp {
	t.Helper()
	stamp, err := Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestStatAgreesWithOsStat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	writeFile(t, file, "hello")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	// The link is followed, as os.Stat follows it, so it reports the file it points to.
	paths := map[string]string{"file": file, "directory": dir, "symbolic link": link, "named pipe": pipe}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			id, ok := Of(info)
			if !ok {
				t.Fatal("os.Stat gave a FileInfo without a device and inode")
			}
			got := mustStat(t, path)
			want := Stamp{
				Size:  info.Size(),
				Mtime: info.ModTime().UnixNano(),
				Ctime: id.Ctime,
				Btime: got.Btime, // Checked on its own, where the system reports it.
				Dev:   id.Dev,
				Ino:   id.Ino,
				Mode:  info.Mode(),
			}
			if got != want {
				t.Errorf("Stat = %+v, want %+v", got, want)
			}
		})
	}
	if got := mustStat(t, file).Size; got != 5 {
		t.Errorf("Size of a 5 byte file = %d", got)
	}
}

func TestStatReportsTheMtimeAProgramSetsButNotTheCtime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeFile(t, path, "data")
	past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	got := mustStat(t, path)
	if got.Mtime != past.UnixNano() {
		t.Errorf("Mtime = %d, want the time set, %d", got.Mtime, past.UnixNano())
	}
	if got.Ctime <= got.Mtime {
		t.Errorf("Ctime = %d is not later than the time a program set, %d", got.Ctime, got.Mtime)
	}
}

func TestStatShowsAWriteInTheMtimeAndNeverMovesTheCtimeBack(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeFile(t, path, "data")
	past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	before := mustStat(t, path)
	writeFile(t, path, "more data")
	after := mustStat(t, path)
	if after.Size == before.Size || after.Mtime == before.Mtime {
		t.Errorf("a write left Size and Mtime as they were: %+v, then %+v", before, after)
	}
	// Equal is possible: the kernel's clock for change times ticks in steps, so two changes
	// close together can share one.
	if after.Ctime < before.Ctime {
		t.Errorf("Ctime went back from %d to %d", before.Ctime, after.Ctime)
	}
}

func TestStatBirthTimeSurvivesAWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeFile(t, path, "data")
	before := mustStat(t, path)
	writeFile(t, path, "other data")
	if after := mustStat(t, path); after.Btime != before.Btime {
		t.Errorf("a write changed Btime from %d to %d", before.Btime, after.Btime)
	}
}

func TestStatGivesAnotherInodeToAFileRenamedOverAnother(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	incoming := filepath.Join(dir, "incoming")
	writeFile(t, target, "same size")
	writeFile(t, incoming, "same size")
	before := mustStat(t, target)
	replacement := mustStat(t, incoming)
	if err := os.Rename(incoming, target); err != nil {
		t.Fatal(err)
	}
	after := mustStat(t, target)
	if after.Ino == before.Ino || after.Ino != replacement.Ino || after.Dev != before.Dev {
		t.Errorf("inode %d, then %d after the rename; the file that replaced it had %d",
			before.Ino, after.Ino, replacement.Ino)
	}
}

func TestStatOfAMissingFileFailsLikeOsStat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"no such name": filepath.Join(dir, "none"), "dangling link": dangling} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Stat(path)
			var pathErr *fs.PathError
			if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) || pathErr.Path != path {
				t.Errorf("Stat(%s) error = %v, want a path error for it wrapping fs.ErrNotExist", path, err)
			}
			if got != (Stamp{}) {
				t.Errorf("Stat(%s) = %+v with an error", path, got)
			}
		})
	}
}
