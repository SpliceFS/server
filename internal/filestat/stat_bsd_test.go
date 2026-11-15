//go:build darwin || freebsd

package filestat

import (
	"syscall"
	"testing"
	"time"
)

func TestStampFromStatReadsEveryField(t *testing.T) {
	t.Parallel()
	var stat syscall.Stat_t
	stat.Size = 5
	stat.Mtimespec = syscall.NsecToTimespec(3*int64(time.Second) + 4)
	setChangeTime(&stat, 5*int64(time.Second)+6)
	stat.Birthtimespec = syscall.NsecToTimespec(7*int64(time.Second) + 8)
	stat.Dev = 9
	stat.Ino = 10
	stat.Mode = syscall.S_IFREG | 0o640
	want := Stamp{
		Size:  5,
		Mtime: 3*int64(time.Second) + 4,
		Ctime: 5*int64(time.Second) + 6,
		Btime: 7*int64(time.Second) + 8,
		Dev:   9,
		Ino:   10,
		Mode:  0o640,
	}
	if got := stampFromStat(&stat); got != want {
		t.Errorf("stampFromStat = %+v, want %+v", got, want)
	}
}

// FreeBSD reports a birth time of -1 seconds for a file system that does not record one.
// The Stamp says 0 for an unknown birth time on every system.
func TestStampFromStatGivesZeroForAnUnreportedBirthTime(t *testing.T) {
	t.Parallel()
	var stat syscall.Stat_t
	stat.Birthtimespec.Sec = -1
	if got := stampFromStat(&stat).Btime; got != 0 {
		t.Errorf("Btime = %d, want 0", got)
	}
}
