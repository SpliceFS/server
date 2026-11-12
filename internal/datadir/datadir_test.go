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

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func put(t *testing.T, dir, name, content string, offset time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch, epoch.Add(offset)); err != nil {
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
	var out []string
	for _, e := range entries {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
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
	if left, _ := os.ReadDir(dir); len(left) != 1 {
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
	if left, _ := os.ReadDir(dir); len(left) != 1 {
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

func TestDirRefresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, buf := newTestDir(dir)

	entries, changed := d.Refresh()
	if len(entries) != 0 || changed {
		t.Errorf("empty dir: %v, %v", entries, changed)
	}
	put(t, dir, "b.yaml", "two", 0)
	put(t, dir, "a.yaml", "one", 0)
	if entries, changed = d.Refresh(); !changed || !reflect.DeepEqual(names(entries), []string{"a.yaml=one", "b.yaml=two"}) {
		t.Errorf("added: %v, %v", names(entries), changed)
	}
	if _, changed = d.Refresh(); changed {
		t.Error("nothing changed but changed = true")
	}

	// Same size and time: not read again. New time or new size: read again.
	put(t, dir, "a.yaml", "ONE", 0)
	if entries, _ = d.Refresh(); names(entries)[0] != "a.yaml=one" {
		t.Errorf("unchanged stamp was read again: %v", names(entries))
	}
	put(t, dir, "a.yaml", "ONE", time.Second)
	if entries, changed = d.Refresh(); !changed || names(entries)[0] != "a.yaml=ONE" {
		t.Errorf("new time: %v, %v", names(entries), changed)
	}
	put(t, dir, "a.yaml", "ONES", time.Second)
	if entries, changed = d.Refresh(); !changed || names(entries)[0] != "a.yaml=ONES" {
		t.Errorf("new size: %v, %v", names(entries), changed)
	}

	if err := os.Remove(filepath.Join(dir, "a.yaml")); err != nil {
		t.Fatal(err)
	}
	if entries, changed = d.Refresh(); !changed || !reflect.DeepEqual(names(entries), []string{"b.yaml=two"}) {
		t.Errorf("removed: %v, %v", names(entries), changed)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log:\n%s", buf)
	}
}

func TestDirBadFileKeepsLastGoodContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, buf := newTestDir(dir)
	put(t, dir, "a.yaml", "good", 0)
	put(t, dir, "b.yaml", "other", 0)
	d.Refresh()

	put(t, dir, "a.yaml", "bad", time.Second)
	entries, changed := d.Refresh()
	if changed || !reflect.DeepEqual(names(entries), []string{"a.yaml=good", "b.yaml=other"}) {
		t.Errorf("after a bad write: %v, %v", names(entries), changed)
	}
	if !strings.Contains(buf.String(), "a.yaml") || strings.Contains(buf.String(), "b.yaml") {
		t.Errorf("log = %s", buf)
	}
	d.Refresh()
	if strings.Count(buf.String(), "a.yaml") != 1 {
		t.Errorf("a file that did not change again was warned about again:\n%s", buf)
	}
	put(t, dir, "a.yaml", "fixed", 2*time.Second)
	if entries, changed = d.Refresh(); !changed || names(entries)[0] != "a.yaml=fixed" {
		t.Errorf("after the fix: %v, %v", names(entries), changed)
	}
	// A file that never was good and goes away is no change.
	put(t, dir, "c.yaml", "bad", 0)
	d.Refresh()
	if err := os.Remove(filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, changed = d.Refresh(); changed {
		t.Error("removing a file that never had a value changed the entries")
	}
}

func TestDirIgnoresOtherFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, buf := newTestDir(dir)
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
	entries, _ := d.Refresh()
	if !reflect.DeepEqual(names(entries), []string{"ok.yaml=fine"}) || buf.Len() != 0 {
		t.Errorf("entries %v, log %s", names(entries), buf)
	}
}

func TestDirMissingAndUnlistable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	d, buf := newTestDir(missing)
	if entries, changed := d.Refresh(); len(entries) != 0 || changed || buf.Len() != 0 {
		t.Errorf("missing dir: %v, %v, %s", entries, changed, buf)
	}

	// A dir that disappears has no files; one that cannot be listed keeps what it had.
	d, buf = newTestDir(dir)
	put(t, dir, "a.yaml", "one", 0)
	d.Refresh()
	d.dir = filepath.Join(dir, "a.yaml")
	entries, changed := d.Refresh()
	if changed || !reflect.DeepEqual(names(entries), []string{"a.yaml=one"}) || !strings.Contains(buf.String(), "cannot list") {
		t.Errorf("unlistable: %v, %v, %s", names(entries), changed, buf)
	}
	d.dir = missing
	if entries, changed = d.Refresh(); len(entries) != 0 || !changed {
		t.Errorf("vanished dir: %v, %v", entries, changed)
	}
}

func TestDirReadFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, buf := newTestDir(dir)
	d.read = func(string) ([]byte, error) { return nil, errors.New("denied") }
	put(t, dir, "a.yaml", "one", 0)
	if entries, _ := d.Refresh(); len(entries) != 0 || !strings.Contains(buf.String(), "denied") {
		t.Errorf("entries %v, log %s", entries, buf)
	}
}
