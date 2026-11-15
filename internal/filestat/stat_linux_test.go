//go:build linux

package filestat

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStampFromStatxReadsEveryField(t *testing.T) {
	t.Parallel()
	stx := unix.Statx_t{
		Mask:      unix.STATX_BASIC_STATS | unix.STATX_BTIME,
		Mode:      syscall.S_IFREG | 0o640,
		Ino:       10,
		Size:      5,
		Mtime:     unix.StatxTimestamp{Sec: 3, Nsec: 4},
		Ctime:     unix.StatxTimestamp{Sec: 5, Nsec: 6},
		Btime:     unix.StatxTimestamp{Sec: 7, Nsec: 8},
		Dev_major: 8,
		Dev_minor: 1,
	}
	want := Stamp{
		Size:  5,
		Mtime: 3*int64(time.Second) + 4,
		Ctime: 5*int64(time.Second) + 6,
		Btime: 7*int64(time.Second) + 8,
		Dev:   8<<8 | 1,
		Ino:   10,
		Mode:  0o640,
	}
	if got := stampFromStatx(&stx); got != want {
		t.Errorf("stampFromStatx = %+v, want %+v", got, want)
	}
}

// A file system that does not record creation times leaves the field unset, and the mask
// says so; whatever the field holds then must not be taken for a time.
func TestStampFromStatxLeavesTheBirthTimeOutWhenTheMaskLacksIt(t *testing.T) {
	t.Parallel()
	stx := unix.Statx_t{
		Mask:  unix.STATX_BASIC_STATS,
		Btime: unix.StatxTimestamp{Sec: 7, Nsec: 8},
	}
	if got := stampFromStatx(&stx).Btime; got != 0 {
		t.Errorf("Btime = %d without STATX_BTIME in the mask, want 0", got)
	}
}

func TestStatxNanosCountsFromTheEpochForward(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		in   unix.StatxTimestamp
		want int64
	}{
		"the epoch":       {unix.StatxTimestamp{}, 0},
		"after the epoch": {unix.StatxTimestamp{Sec: 2, Nsec: 500_000_000}, 2_500_000_000},
		// The kernel keeps the nanoseconds positive and moves the seconds back.
		"before the epoch": {unix.StatxTimestamp{Sec: -2, Nsec: 500_000_000}, -1_500_000_000},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := statxNanos(tt.in); got != tt.want {
				t.Errorf("statxNanos(%+v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// Whether the file system records creation times depends on the machine, so this asks the
// kernel and checks that Stat gives the same answer.
func TestStatReportsTheBirthTimeWhenTheKernelDoes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeFile(t, path, "data")
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &stx); err != nil {
		t.Fatal(err)
	}
	want := int64(0)
	if stx.Mask&unix.STATX_BTIME != 0 {
		want = statxNanos(stx.Btime)
		if want == 0 {
			t.Fatal("the kernel reports a birth time of 0 for a new file")
		}
	}
	if got := mustStat(t, path).Btime; got != want {
		t.Errorf("Btime = %d, want %d", got, want)
	}
}
