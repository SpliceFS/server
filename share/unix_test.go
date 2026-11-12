//go:build unix

package share

import (
	"errors"
	"path/filepath"
	"reflect"
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
	f := newFixture(t)
	f.write(t, "regular", "x", epoch)
	if err := mkfifo(filepath.Join(f.root, "pipe")); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
	s := f.open(t, Options{})
	list, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"regular"}; !reflect.DeepEqual(names(list), want) {
		t.Errorf("List = %v, want %v", names(list), want)
	}
	if _, err := s.Stat("pipe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(pipe) = %v", err)
	}
	if _, err := s.OpenFile("pipe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenFile(pipe) = %v", err)
	}
	if _, _, err := s.HeadDigest("pipe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HeadDigest(pipe) = %v", err)
	}
	if _, err := s.List("pipe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(pipe) = %v", err)
	}
}
