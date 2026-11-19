package digestcache

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpliceFS/server/internal/filestat"
)

// files is a share as a sweep sees it: the stamps of the visible files by path. A path
// that is missing is gone or hidden; failing fails the lookup.
type files struct {
	stamps  map[string]filestat.Stamp
	failing map[string]bool
	// asked, when set, is called with every path asked about.
	asked func(path string)
}

func (f files) Stamp(path string) (filestat.Stamp, bool, error) {
	if f.asked != nil {
		f.asked(path)
	}
	if f.failing[path] {
		return filestat.Stamp{}, false, errors.New("cannot tell")
	}
	stamp, ok := f.stamps[path]
	return stamp, ok, nil
}

// entryAt is where the entry of the file at path in share is in dir.
func entryAt(dir, path string) string {
	name, _ := entryPath(path) //nolint:errcheck // the tests use clean paths only
	return filepath.Join(dir, share, filepath.FromSlash(name))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// sweepWith saves an entry of size 1 for each of paths in share, sweeps with the share's
// files and returns the folder of the store.
func sweepWith(t *testing.T, paths []string, current files) string {
	t.Helper()
	dir := t.TempDir()
	store := openStore(t, dir)
	for _, path := range paths {
		save(t, store, share, path, 1)
	}
	if err := store.Sweep(context.Background(), map[string]Files{share: current}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepKeepsCurrent(t *testing.T) {
	t.Parallel()
	paths := []string{"a", "d/b", "100%/caf\xe9"}
	current := files{stamps: map[string]filestat.Stamp{}}
	for _, path := range paths {
		current.stamps[path] = testStamp(1)
	}
	dir := sweepWith(t, paths, current)
	for _, path := range paths {
		if !exists(entryAt(dir, path)) {
			t.Errorf("the entry of %q is gone", path)
		}
	}
}

func TestSweepRemovesGone(t *testing.T) {
	t.Parallel()
	dir := sweepWith(t, []string{"a", "d/e/gone"}, files{stamps: map[string]filestat.Stamp{"a": testStamp(1)}})
	if exists(entryAt(dir, "d/e/gone")) || exists(filepath.Join(dir, share, "d")) {
		t.Error("the entry of a file that is gone, or its empty folders, are still there")
	}
	if !exists(entryAt(dir, "a")) {
		t.Error("the entry of a current file is gone")
	}
}

// A file the share hides answers as a missing one does; the share decides which are hidden.
func TestSweepRemovesHidden(t *testing.T) {
	t.Parallel()
	dir := sweepWith(t, []string{".hidden"}, files{stamps: map[string]filestat.Stamp{}})
	if exists(entryAt(dir, ".hidden")) {
		t.Error("the entry of a hidden file is still there")
	}
}

func TestSweepRemovesChanged(t *testing.T) {
	t.Parallel()
	dir := sweepWith(t, []string{"a"}, files{stamps: map[string]filestat.Stamp{"a": testStamp(2)}})
	if exists(entryAt(dir, "a")) {
		t.Error("the entry of a changed file is still there")
	}
}

func TestSweepRemovesUnconfiguredShare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, "old", "a", 1)
	if err := store.Sweep(context.Background(), map[string]Files{}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "old")) {
		t.Error("the folder of a share that is not configured is still there")
	}
	if !exists(filepath.Join(dir, tempFolder)) {
		t.Error("the sweep removed the temporary folder")
	}
}

// What is not an entry the server wrote where it is found goes; an entry the share cannot
// answer for stays, since it may still be right.
func TestSweepRemovesWhatIsNotAnEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, share, "kept", 1)
	save(t, store, share, "moved", 1)
	shareDir := filepath.Join(dir, share)
	if err := os.Rename(entryAt(dir, "moved"), filepath.Join(shareDir, "elsewhere"+stateSuffix)); err != nil {
		t.Fatal(err)
	}
	written := string(newEntry("x", testStamp(1), testDigests(1)).encode())
	strays := map[string]string{
		"corrupt" + stateSuffix:  "not an entry",
		"badpath" + stateSuffix:  strings.Replace(written, "path: x", "path: '%zz'", 1),
		"notclean" + stateSuffix: strings.Replace(written, "path: x", "path: a//b", 1),
	}
	for name, content := range strays {
		if err := os.WriteFile(filepath.Join(shareDir, name), []byte(content), fileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("missing", filepath.Join(shareDir, "dangling"+stateSuffix)); err != nil {
		t.Fatal(err)
	}
	current := files{
		stamps:  map[string]filestat.Stamp{"moved": testStamp(1), "x": testStamp(1)},
		failing: map[string]bool{"kept": true},
	}
	if err := store.Sweep(context.Background(), map[string]Files{share: current}); err != nil {
		t.Fatal(err)
	}
	if left := names(t, shareDir); len(left) != 1 || left[0] != "kept"+stateSuffix {
		t.Errorf("left %v, want only the entry the share could not answer for", left)
	}
}

// A cancelled sweep stops before it removes anything more.
func TestSweepStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, share, "a", 1)
	save(t, store, "old", "a", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := store.Sweep(ctx, map[string]Files{share: files{}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Sweep = %v, want the cancellation", err)
	}
	if !exists(entryAt(dir, "a")) || !exists(filepath.Join(dir, "old")) {
		t.Error("a cancelled sweep removed entries")
	}
}

// A sweep stops between two entries when it is cancelled.
func TestSweepStopsBetweenEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, share, "a", 1)
	save(t, store, share, "b", 1)
	ctx, cancel := context.WithCancel(context.Background())
	asked := 0
	current := files{asked: func(string) { asked++; cancel() }}
	if err := store.Sweep(ctx, map[string]Files{share: current}); !errors.Is(err, context.Canceled) {
		t.Errorf("Sweep = %v, want the cancellation", err)
	}
	if asked != 1 || len(names(t, filepath.Join(dir, share))) != 1 {
		t.Errorf("asked about %d files, want 1 and one entry removed", asked)
	}
}

// A file where the folder of a configured share goes is logged and left alone.
func TestSweepLogsAShareFolderItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	if err := os.WriteFile(filepath.Join(dir, share), nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(context.Background(), map[string]Files{share: files{}}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, share)) {
		t.Error("the sweep removed what it could not read")
	}
}

func TestSweepFailsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(context.Background(), nil); err == nil {
		t.Error("Sweep = nil, want an error")
	}
}
