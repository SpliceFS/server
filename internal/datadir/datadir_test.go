package datadir

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestNodeID(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"0123456789abcdef": true, "ffffffffffffffff": true,
		"": false, "0123456789abcde": false, "0123456789abcdef0": false,
		"0123456789ABCDEF": false, "0123456789abcdeg": false, "0123456789abcde/": false,
		"0123456789abcdef\n": false,
		//nolint:gocritic // the trailing space is the case under test
		"0123456789abcde ": false,
	}
	for id, want := range tests {
		if got := IsNodeID(id); got != want {
			t.Errorf("IsNodeID(%q) = %v, want %v", id, got, want)
		}
	}
	if got := NodeFile("0123456789abcdef"); got != "0123456789abcdef.yaml" {
		t.Errorf("NodeFile = %q", got)
	}
	if id, ok := NodeOfFile("0123456789abcdef.yaml"); !ok || id != "0123456789abcdef" {
		t.Errorf("NodeOfFile = %q, %v", id, ok)
	}
	for _, name := range []string{"x.yaml", "0123456789abcdef", "0123456789abcdef.yml", ".yaml"} {
		if _, ok := NodeOfFile(name); ok {
			t.Errorf("NodeOfFile(%q) accepted", name)
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
	put(t, dir, "a.yaml", "one", 0)
	follower.Refresh()

	// Same size and time: not read again. New time or new size: read again.
	put(t, dir, "a.yaml", "ONE", 0)
	entries, changed := follower.Refresh()
	if changed {
		t.Error("unchanged stamp was read again: changed = true")
	}
	checkNames(t, "same size and time", entries, "a.yaml=one")
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
