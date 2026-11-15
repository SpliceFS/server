//go:build unix

package share

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/SpliceFS/common/wire"
)

type wireSpace = wire.Space

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }

func errSyscall(name string) error {
	return map[string]error{"ENOTDIR": syscall.ENOTDIR, "ELOOP": syscall.ELOOP}[name]
}

func TestNamedPipeIsHidden(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "regular", "x", epoch())
	if err := mkfifo(filepath.Join(fix.root, "pipe")); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
	share := fix.open(t, &Options{})
	requireListing(t, share, "", []string{"regular"})
	requireHidden(t, share, "pipe")
}

func TestNonNegative(t *testing.T) {
	t.Parallel()
	if got := nonNegative(int64(-1)); got != 0 {
		t.Errorf("nonNegative(-1) = %d, want 0", got)
	}
	if got := nonNegative(int32(4096)); got != 4096 {
		t.Errorf("nonNegative(4096) = %d", got)
	}
	if got := nonNegative(uint32(7)); got != 7 {
		t.Errorf("nonNegative(7) = %d", got)
	}
}
