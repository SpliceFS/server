package datadir

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/server/internal/filestat"
)

// epoch is the fixed point in time that the tests measure modification times from.
func epoch() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func put(t *testing.T, dir, name, content string, offset time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch(), epoch().Add(offset)); err != nil {
		t.Fatal(err)
	}
}

// replace writes a file the way the server's own writers do, through a temporary file
// renamed over the old one, and then gives it the modification time epoch plus offset.
func replace(t *testing.T, dir, name, content string, offset time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := WriteAtomic(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch(), epoch().Add(offset)); err != nil {
		t.Fatal(err)
	}
}

// parseWord accepts files whose content is not "bad" and returns the content.
func parseWord(_ string, data []byte) (string, error) {
	if string(data) == "bad" {
		return "", errors.New("bad content")
	}
	return string(data), nil
}

func newTestDir(dir string) (*Dir[string], *bytes.Buffer) {
	var buf bytes.Buffer
	return NewDir(dir, parseWord, slog.New(slog.NewTextHandler(&buf, nil))), &buf
}

func names(entries []Entry[string]) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name+"="+entry.Value)
	}
	return out
}

// listDir returns the entries of dir.
func listDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// checkNames fails the test when entries do not have the names and values in want, in order.
func checkNames(t *testing.T, what string, entries []Entry[string], want ...string) {
	t.Helper()
	if got := names(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: entries = %v, want %v", what, got, want)
	}
}

// checkNoLog fails the test when the logger wrote anything to buf.
func checkNoLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if buf.Len() != 0 {
		t.Errorf("unexpected log:\n%s", buf)
	}
}

func TestWriteAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	for _, content := range []string{"first", "second"} {
		if err := WriteAtomic(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || string(data) != content || statErr != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("file = %q, %v, %v", data, info, err)
		}
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestWriteAtomicFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteAtomic(filepath.Join(dir, "missing", "f"), nil, 0o600); err == nil {
		t.Error("write into a missing folder succeeded")
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(target, []byte("x"), 0o600); err == nil {
		t.Error("rename over a folder succeeded")
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("a failed write left a temporary file: %v", left)
	}
}

func TestWriteOnceWritesWithItsMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := WriteOnce(path, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || string(data) != "first" || statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("file = %q, %v, %v", data, info, err)
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("a temporary file is left: %v", left)
	}
}

func TestWriteOnceNeverReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := WriteOnce(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteOnce(path, []byte("second"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("second write = %v, want an error that is fs.ErrExist", err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "first" {
		t.Errorf("file = %q, %v, want the first content", data, readErr)
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("a refused write left a temporary file: %v", left)
	}
}

// A temporary file that a crash left is not the final file and does not stop the write.
func TestWriteOnceIgnoresAStaleTemporaryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, tempPrefix+"stale", "half a file", 0)
	path := filepath.Join(dir, "f")
	if err := WriteOnce(path, []byte("whole"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "whole" {
		t.Errorf("file = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, tempPrefix+"stale")); err != nil {
		t.Errorf("the stale file of another writer was removed: %v", err)
	}
}

// Every writer gets the same start signal and none waits for another, so the writes overlap
// as much as the machine lets them. Whatever the order, one write wins and the others see
// that the file exists.
func TestWriteOnceHasOneWinner(t *testing.T) {
	t.Parallel()
	const writers = 16
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	start := make(chan struct{})
	errs := make([]error, writers)
	var done sync.WaitGroup
	for i := range writers {
		done.Go(func() {
			<-start
			errs[i] = WriteOnce(path, []byte(strconv.Itoa(i)), 0o600)
		})
	}
	close(start)
	done.Wait()

	winner := -1
	for i, err := range errs {
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			t.Errorf("writer %d: %v, want nil or fs.ErrExist", i, err)
			continue
		}
		if winner >= 0 {
			t.Errorf("writers %d and %d both succeeded", winner, i)
		}
		winner = i
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != strconv.Itoa(winner) {
		t.Errorf("file = %q, %v, want the content of writer %d", data, err, winner)
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("temporary files are left: %v", left)
	}
}

// These failures do not depend on the user: a missing folder and a folder where the file
// belongs fail for root as well.
func TestWriteOnceFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteOnce(filepath.Join(dir, "missing", "f"), nil, 0o600); err == nil {
		t.Error("write into a missing folder succeeded")
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteOnce(target, []byte("x"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Errorf("write over a folder = %v, want fs.ErrExist", err)
	}
	if left := listDir(t, dir); len(left) != 1 {
		t.Errorf("a failed write left a temporary file: %v", left)
	}
}

func TestNodeFile(t *testing.T) {
	t.Parallel()
	const id = "017f22e2-79b0-7cc3-98c4-dc0c0c07398f"
	if got := IDFile(id); got != id+".yaml" {
		t.Errorf("IDFile = %q", got)
	}
	if got, ok := IDOfFile(id + ".yaml"); !ok || got != id {
		t.Errorf("IDOfFile = %q, %v", got, ok)
	}
	for _, name := range []string{
		"x.yaml", id, id + ".yml", ".yaml", strings.ToUpper(id) + ".yaml",
		"0123456789abcdef.yaml", // the id of the earlier form
	} {
		if _, ok := IDOfFile(name); ok {
			t.Errorf("IDOfFile(%q) accepted", name)
		}
	}
}

func TestDirRefreshFindsFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)

	entries, changed := follower.Refresh()
	if len(entries) != 0 || changed {
		t.Errorf("empty dir: %v, %v", entries, changed)
	}
	put(t, dir, "b.yaml", "two", 0)
	put(t, dir, "a.yaml", "one", 0)
	entries, changed = follower.Refresh()
	if !changed {
		t.Error("files were added but changed = false")
	}
	checkNames(t, "added", entries, "a.yaml=one", "b.yaml=two")
	if _, changed = follower.Refresh(); changed {
		t.Error("nothing changed but changed = true")
	}
	checkNoLog(t, buf)
}

func TestDirRefreshReadsOnlyChangedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	var reads int
	follower.read = func(path string) ([]byte, error) {
		reads++
		return os.ReadFile(path)
	}
	put(t, dir, "a.yaml", "one", 0)
	follower.Refresh()

	// An untouched file is not read again. New time or new size: read again.
	entries, changed := follower.Refresh()
	if changed || reads != 1 {
		t.Errorf("untouched file: changed = %v after %d reads, want false after 1", changed, reads)
	}
	checkNames(t, "untouched", entries, "a.yaml=one")
	put(t, dir, "a.yaml", "ONE", time.Second)
	entries, changed = follower.Refresh()
	if !changed {
		t.Error("new time: changed = false")
	}
	checkNames(t, "new time", entries, "a.yaml=ONE")
	put(t, dir, "a.yaml", "ONES", time.Second)
	entries, changed = follower.Refresh()
	if !changed {
		t.Error("new size: changed = false")
	}
	checkNames(t, "new size", entries, "a.yaml=ONES")
	checkNoLog(t, buf)
}

// A file replaced by one of the same size and the same modification time is the case a
// coarse clock, or a sync tool that keeps the time, makes. The new file has another inode,
// whatever the clock says.
func TestDirRefreshSeesFileReplacedWithSameSizeAndTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	put(t, dir, "a.yaml", "one", 0)
	follower.Refresh()

	replace(t, dir, "a.yaml", "two", 0)
	entries, changed := follower.Refresh()
	if !changed {
		t.Error("a file of the same size and time was replaced but changed = false")
	}
	checkNames(t, "replaced", entries, "a.yaml=two")
	if _, changed = follower.Refresh(); changed {
		t.Error("nothing changed after the replacement but changed = true")
	}
	checkNoLog(t, buf)
}

// The change time of a file rewritten in place cannot be set by the test, because it
// is the clock's, so the identity is set by the test and the file keeps its size and time.
func TestDirStampHoldsTheIdentityOfTheFile(t *testing.T) {
	t.Parallel()
	base := filestat.Identity{Dev: 1, Ino: 2, Ctime: 3}
	tests := map[string]struct {
		id          filestat.Identity
		wantChanged bool
		want        string
	}{
		"nothing":     {id: base, want: "a.yaml=one"},
		"device":      {id: filestat.Identity{Dev: 9, Ino: 2, Ctime: 3}, wantChanged: true, want: "a.yaml=two"},
		"inode":       {id: filestat.Identity{Dev: 1, Ino: 9, Ctime: 3}, wantChanged: true, want: "a.yaml=two"},
		"change time": {id: filestat.Identity{Dev: 1, Ino: 2, Ctime: 9}, wantChanged: true, want: "a.yaml=two"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			follower, buf := newTestDir(dir)
			current := base
			follower.identify = func(fs.FileInfo) (filestat.Identity, bool) { return current, true }
			put(t, dir, "a.yaml", "one", 0)
			follower.Refresh()

			current = test.id
			put(t, dir, "a.yaml", "two", 0)
			entries, changed := follower.Refresh()
			if changed != test.wantChanged {
				t.Errorf("changed = %v, want %v", changed, test.wantChanged)
			}
			checkNames(t, name, entries, test.want)
			checkNoLog(t, buf)
		})
	}
}

// A reader that keeps a value between calls compares the stamp of the file with the stamp the
// value was loaded at, so an entry holds the stamp of the file as Dir last looked at it.
func TestEntryHoldsTheStampOfItsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	path := filepath.Join(dir, "a.yaml")
	stampNow := func() Stamp {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return StampOf(info)
	}
	put(t, dir, "a.yaml", "good", 0)
	first := stampNow()
	entries, _ := follower.Refresh()
	if len(entries) != 1 || entries[0].Stamp != first {
		t.Fatalf("entries = %+v, want the stamp %+v", entries, first)
	}
	// A bad write keeps the last good value; the stamp is that of the bad file, which is the
	// file Dir has looked at, so it is not read again until it changes.
	put(t, dir, "a.yaml", "bad", time.Second)
	second := stampNow()
	entries, _ = follower.Refresh()
	if second == first || entries[0].Value != "good" || entries[0].Stamp != second {
		t.Errorf("after a bad write: %+v, want the value good with the stamp %+v", entries, second)
	}
	checkNames(t, "bad", entries, "a.yaml=good")
	if got := strings.Count(buf.String(), "a.yaml"); got != 1 {
		t.Errorf("the bad file was warned about %d times, want once", got)
	}
}

func TestStampOfTellsVersionsOfAFileApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")
	stampOfFile := func() Stamp {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return StampOf(info)
	}
	put(t, dir, "a.yaml", "one", 0)
	first := stampOfFile()
	if again := stampOfFile(); again != first {
		t.Errorf("the same file has two stamps: %+v and %+v", first, again)
	}
	put(t, dir, "a.yaml", "two", time.Second)
	if changed := stampOfFile(); changed == first {
		t.Errorf("a changed file has the stamp %+v again", first)
	}
}

func TestDirRefreshDropsRemovedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	put(t, dir, "b.yaml", "two", 0)
	put(t, dir, "a.yaml", "one", 0)
	follower.Refresh()

	if err := os.Remove(filepath.Join(dir, "a.yaml")); err != nil {
		t.Fatal(err)
	}
	entries, changed := follower.Refresh()
	if !changed {
		t.Error("a file was removed but changed = false")
	}
	checkNames(t, "removed", entries, "b.yaml=two")
	checkNoLog(t, buf)
}

func TestDirBadFileKeepsLastGoodContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	put(t, dir, "a.yaml", "good", 0)
	put(t, dir, "b.yaml", "other", 0)
	follower.Refresh()

	put(t, dir, "a.yaml", "bad", time.Second)
	entries, changed := follower.Refresh()
	if changed || !reflect.DeepEqual(names(entries), []string{"a.yaml=good", "b.yaml=other"}) {
		t.Errorf("after a bad write: %v, %v", names(entries), changed)
	}
	if !strings.Contains(buf.String(), "a.yaml") || strings.Contains(buf.String(), "b.yaml") {
		t.Errorf("log = %s", buf)
	}
	follower.Refresh()
	if strings.Count(buf.String(), "a.yaml") != 1 {
		t.Errorf("a file that did not change again was warned about again:\n%s", buf)
	}
	put(t, dir, "a.yaml", "fixed", 2*time.Second)
	if entries, changed = follower.Refresh(); !changed || names(entries)[0] != "a.yaml=fixed" {
		t.Errorf("after the fix: %v, %v", names(entries), changed)
	}
	// A file that never was good and goes away is no change.
	put(t, dir, "c.yaml", "bad", 0)
	follower.Refresh()
	if err := os.Remove(filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, changed = follower.Refresh(); changed {
		t.Error("removing a file that never had a value changed the entries")
	}
}

func TestDirIgnoresOtherFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	for _, name := range []string{".a.yaml", ".tmp-1", "a.txt", "a.yaml~", "a.yml", "yaml"} {
		put(t, dir, name, "x", 0)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "ok.yaml", "fine", 0)
	entries, _ := follower.Refresh()
	if !reflect.DeepEqual(names(entries), []string{"ok.yaml=fine"}) || buf.Len() != 0 {
		t.Errorf("entries %v, log %s", names(entries), buf)
	}
}

func TestDirMissingAndUnlistable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	follower, buf := newTestDir(missing)
	if entries, changed := follower.Refresh(); len(entries) != 0 || changed || buf.Len() != 0 {
		t.Errorf("missing dir: %v, %v, %s", entries, changed, buf)
	}

	// A dir that disappears has no files; one that cannot be listed keeps what it had.
	follower, buf = newTestDir(dir)
	put(t, dir, "a.yaml", "one", 0)
	follower.Refresh()
	follower.dir = filepath.Join(dir, "a.yaml")
	entries, changed := follower.Refresh()
	if changed || !strings.Contains(buf.String(), "cannot list") {
		t.Errorf("unlistable: %v, %v, %s", names(entries), changed, buf)
	}
	checkNames(t, "unlistable", entries, "a.yaml=one")
	follower.dir = missing
	if entries, changed = follower.Refresh(); len(entries) != 0 || !changed {
		t.Errorf("vanished dir: %v, %v", entries, changed)
	}
}

func TestDirReadFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	follower, buf := newTestDir(dir)
	follower.read = func(string) ([]byte, error) { return nil, errors.New("denied") }
	put(t, dir, "a.yaml", "one", 0)
	if entries, _ := follower.Refresh(); len(entries) != 0 || !strings.Contains(buf.String(), "denied") {
		t.Errorf("entries %v, log %s", entries, buf)
	}
}
