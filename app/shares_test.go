package app

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/units"
	"github.com/SpliceFS/server/share"
)

func TestShareAdapterOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	set, err := openShares(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	media := set.byName["media"]

	file, err := media.Open("hello.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "0123456789" {
		t.Errorf("read %q, %v", data, err)
	}
	if info := file.Info(); info.Size != 10 || info.ETag != share.ETag(10, fileTime) {
		t.Errorf("info = %+v", info)
	}

	missing, err := media.Open("nope")
	if !errors.Is(err, share.ErrNotFound) {
		t.Errorf("err = %v, want share.ErrNotFound", err)
	}
	if missing != nil {
		t.Errorf("a failed Open returned the non-nil value %v", missing)
	}
}

func TestOpenSharesAppliesSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Shares["media"] = config.Share{Path: f.shareDir, Exclude: []string{"*.html"}, MinAge: units.Duration(1 << 62)}
	set, err := openShares(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	// Everything is newer than the minimum age, so the share looks empty.
	infos, err := set.byName["media"].List("")
	if err != nil || len(infos) != 0 {
		t.Errorf("List = %v, %v", infos, err)
	}
}

func TestOpenSharesFailureClosesTheOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Shares["zzz"] = config.Share{Path: f.shareDir + "/missing"}
	set, err := openShares(f.cfg)
	if err == nil || set != nil || !strings.Contains(err.Error(), `share "zzz"`) {
		t.Errorf("openShares = %v, %v", set, err)
	}
}

func TestOpenSharesRejectsBadExcludePattern(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Shares["media"] = config.Share{Path: f.shareDir, Exclude: []string{"["}}
	if _, err := openShares(f.cfg); err == nil || !strings.Contains(err.Error(), "exclude pattern") {
		t.Errorf("err = %v", err)
	}
}

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestShareSetCloseReportsEveryFailure(t *testing.T) {
	t.Parallel()
	first, second := errors.New("first"), errors.New("second")
	set := &shareSet{closers: []io.Closer{failingCloser{first}, failingCloser{nil}, failingCloser{second}}}
	err := set.Close()
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Errorf("Close = %v", err)
	}
	if err := (&shareSet{}).Close(); err != nil {
		t.Errorf("Close of an empty set = %v", err)
	}
}
