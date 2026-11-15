//go:build unix

package filestat

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// statInfo is a file description whose Sys value a test chooses.
type statInfo struct {
	fs.FileInfo

	sys any
}

func (info statInfo) Sys() any { return info.sys }

func TestOfReadsWhatTheFileSystemReports(t *testing.T) {
	t.Parallel()
	var stat syscall.Stat_t
	stat.Dev = 1
	stat.Ino = 2
	setChangeTime(&stat, 3*int64(time.Second)+4)
	got, ok := Of(statInfo{sys: &stat})
	if want := (Identity{Dev: 1, Ino: 2, Ctime: 3*int64(time.Second) + 4}); !ok || got != want {
		t.Errorf("Of = %+v, %v, want %+v", got, ok, want)
	}
}

func TestOfNeedsTheFileSystemsOwnDescription(t *testing.T) {
	t.Parallel()
	for name, sys := range map[string]any{"none": nil, "other type": "not a stat"} {
		if got, ok := Of(statInfo{sys: sys}); ok || got != (Identity{}) {
			t.Errorf("sys %s: Of = %+v, %v", name, got, ok)
		}
	}
}

func TestOfRealFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]Identity{}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := Of(info)
		if !ok || id.Ino == 0 || id.Ctime == 0 {
			t.Fatalf("Of(%s) = %+v, %v", path, id, ok)
		}
		ids[path] = id
	}
	if ids[first] == ids[second] {
		t.Errorf("two files that exist together have the same identity %+v", ids[first])
	}
}

func TestAsUint64KeepsDifferentNumbersDifferent(t *testing.T) {
	t.Parallel()
	if asUint64(int32(-1)) == asUint64(int32(0)) || asUint64(int32(-1)) == asUint64(int32(1)) {
		t.Error("a negative device number equals another one")
	}
	if got := asUint64(uint32(7)); got != 7 {
		t.Errorf("asUint64(7) = %d", got)
	}
}
