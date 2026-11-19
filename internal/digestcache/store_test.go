package digestcache

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/SpliceFS/server/internal/filestat"
)

// share is the share the tests keep their entries in.
const share = "photos"

// testStamp is a stamp of a file; size tells versions of one file apart.
func testStamp(size int64) filestat.Stamp {
	return filestat.Stamp{Size: size, Mtime: 1763452800123456789, Ctime: 1763452801000000000, Dev: 7, Ino: 81273}
}

// testDigests are the digests of the version of a file of size, different for every size.
func testDigests(size int64) Digests {
	return Digests{Head: fmt.Sprintf("a%063x", size), Tail: fmt.Sprintf("b%063x", size)}
}

// testEntry is the entry Save writes for the file at path with the stamp of size.
func testEntry(path string, size int64) entry {
	return newEntry(path, testStamp(size), testDigests(size))
}

// save saves the entry of the file at path in shareName with the stamp of size.
func save(t *testing.T, store *Store, shareName, path string, size int64) {
	t.Helper()
	if err := store.Save(shareName, path, testStamp(size), testDigests(size)); err != nil {
		t.Errorf("Save(%q, %q) = %v", shareName, path, err)
	}
}

// load reports whether the entry of the file at path in shareName answers for the stamp of
// size with the digests of size.
func load(store *Store, shareName, path string, size int64) bool {
	got, ok := store.Load(shareName, path, testStamp(size))
	return ok && got == testDigests(size)
}

// openRoot opens dir as a root that is closed when the test ends.
func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

// openStore opens a store on dir, as the server does at start, closed when the test ends.
func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

// names lists the folder at path, failing the test when it cannot.
func names(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	return found
}

// checkTempEmpty fails the test when a temporary file was left behind.
func checkTempEmpty(t *testing.T, dir string) {
	t.Helper()
	if left := names(t, filepath.Join(dir, ".tmp")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestSaveThenLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	paths := []string{"a.jpg", "photos/2024/a.jpg", "100%/caf\xe9", strings.Repeat("a", 300) + "/b"}
	for i, path := range paths {
		size := int64(i)
		save(t, store, share, path, size)
		if !load(store, share, path, size) {
			t.Errorf("Load(%q) misses what Save wrote", path)
		}
		if got, ok := store.Load("other", path, testStamp(size)); ok {
			t.Errorf("Load of another share = %+v", got)
		}
	}
	if got, ok := store.Load(share, "never-saved", testStamp(0)); ok {
		t.Errorf("Load of a path never saved = %+v", got)
	}
	checkTempEmpty(t, dir)
}

// mustMake returns a setup that makes the file with create.
func mustMake(create func(file string) error) func(*testing.T, string) {
	return func(t *testing.T, file string) {
		t.Helper()
		if err := create(file); err != nil {
			t.Fatal(err)
		}
	}
}

// writeEntry returns a setup that writes data as the entry file.
func writeEntry(data []byte) func(*testing.T, string) {
	return mustMake(func(file string) error { return os.WriteFile(file, data, 0o600) })
}

// linkOutside returns a setup that makes the entry file a link to a file with data outside
// the digest folder.
func linkOutside(data []byte) func(*testing.T, string) {
	return func(t *testing.T, file string) {
		t.Helper()
		outside := filepath.Join(t.TempDir(), "outside")
		writeEntry(data)(t, outside)
		if err := os.Symlink(outside, file); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadCorruptIsMiss(t *testing.T) {
	t.Parallel()
	valid := testEntry("a", 1).encode()
	tests := map[string]struct {
		path  string
		setup func(t *testing.T, file string)
	}{
		"missing":          {"a", func(*testing.T, string) {}},
		"empty":            {"a", writeEntry(nil)},
		"not YAML":         {"a", writeEntry([]byte("{"))},
		"cut short":        {"a", writeEntry(valid[:len(valid)/2])},
		"another version":  {"a", writeEntry([]byte(strings.Replace(string(valid), "version: 1", "version: 2", 1)))},
		"a malformed head": {"a", writeEntry([]byte(strings.Replace(string(valid), "head: a", "head: A", 1)))},
		"a folder":         {"a", mustMake(func(file string) error { return os.Mkdir(file, 0o700) })},
		// Opening a FIFO waits for a writer unless it is opened without blocking.
		"a FIFO": {"a", mustMake(func(file string) error { return syscall.Mkfifo(file, 0o600) })},
		// The entry it points at is valid, so only the root keeps it out.
		"a link out of the folder": {"a", linkOutside(valid)},
		"a path that is not clean": {"b/../a", writeEntry(valid)},
		// Two paths can come to one entry file (rule 4, or a file system that folds case).
		"the entry of another path":  {"a", writeEntry(testEntry("A", 1).encode())},
		"the entry of another stamp": {"a", writeEntry(testEntry("a", 2).encode())},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store := openStore(t, dir)
			if err := os.Mkdir(filepath.Join(dir, share), 0o700); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, filepath.Join(dir, share, "a.splicefs_state"))
			if got, ok := store.Load(share, tt.path, testStamp(1)); ok {
				t.Errorf("Load(%q) = %+v, want a miss", tt.path, got)
			}
		})
	}
}

func TestSaveModes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, share, "a/b/c.jpg", 1)
	walked := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if entry.IsDir() {
			want = fs.ModeDir | 0o700
		}
		if info.Mode() != want {
			t.Errorf("%s has mode %v, want %v", path, info.Mode(), want)
		}
		walked++
		return nil
	})
	// .tmp, photos, photos/a, photos/a/b and the entry.
	if err != nil || walked != 5 {
		t.Errorf("walked %d paths, %v", walked, err)
	}
}

func TestSaveReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	for _, size := range []int64{1, 2} {
		save(t, store, share, "a", size)
	}
	if load(store, share, "a", 1) || !load(store, share, "a", 2) {
		t.Error("Load does not answer with the second entry only")
	}
	if found := names(t, filepath.Join(dir, share)); len(found) != 1 {
		t.Errorf("share folder holds %v", found)
	}
	checkTempEmpty(t, dir)
}

func TestSaveFailures(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		path  string
		setup func(t *testing.T, dir string) error
	}{
		"a path that is not clean": {"a/", func(*testing.T, string) error { return nil }},
		"the share folder is a file": {"a", func(_ *testing.T, dir string) error {
			return os.WriteFile(filepath.Join(dir, share), nil, 0o600)
		}},
		"the temporary folder is gone": {"a", func(_ *testing.T, dir string) error {
			return os.Remove(filepath.Join(dir, ".tmp"))
		}},
		"a folder is where the entry goes": {"a", func(_ *testing.T, dir string) error {
			return os.MkdirAll(filepath.Join(dir, share, "a.splicefs_state", "x"), 0o700)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store := openStore(t, dir)
			if err := tt.setup(t, dir); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(share, tt.path, testStamp(1), testDigests(1)); err == nil {
				t.Errorf("Save(%q) succeeded", tt.path)
			}
			if _, err := os.Stat(filepath.Join(dir, ".tmp")); err == nil {
				checkTempEmpty(t, dir)
			}
		})
	}
}

// Writers of the same and of different entries never leave a partial entry or a temporary
// file: each writes its own temporary file and renames it into place.
func TestSaveConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	paths := []string{"a", "b/c", "b/d", "e/f/g"}
	shares := []string{share, "music"}
	const writers = 16
	var group sync.WaitGroup
	for writer := range writers {
		group.Go(func() {
			for _, shareName := range shares {
				saveAll(t, store, shareName, paths, int64(writer))
			}
		})
	}
	group.Wait()
	for _, shareName := range shares {
		for _, path := range paths {
			if answering := answeringSizes(store, shareName, path, writers); answering != 1 {
				t.Errorf("Load(%q, %q) answers for %d stamps, want the one of the last writer", shareName, path, answering)
			}
		}
	}
	checkTempEmpty(t, dir)
}

// saveAll saves the entry of size for every path in shareName.
func saveAll(t *testing.T, store *Store, shareName string, paths []string, size int64) {
	t.Helper()
	for _, path := range paths {
		save(t, store, shareName, path, size)
	}
}

// answeringSizes counts the sizes below limit whose stamp the entry of path answers for.
func answeringSizes(store *Store, shareName, path string, limit int64) int {
	answering := 0
	for size := range limit {
		if load(store, shareName, path, size) {
			answering++
		}
	}
	return answering
}

func TestStartEmptiesTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	save(t, openStore(t, dir), share, "a", 1)
	for _, left := range []string{"left", "sub/deeper"} {
		path := filepath.Join(dir, ".tmp", left)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := openStore(t, dir)
	checkTempEmpty(t, dir)
	if !load(store, share, "a", 1) {
		t.Error("the entry saved before the start is gone")
	}
}

func TestStartMakesTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	openStore(t, dir)
	checkTempEmpty(t, dir)
}

// A digest folder that was removed after it was opened cannot take a temporary folder.
func TestStartFailsWithoutFolder(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "digests")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if store, err := start(dir, root, slog.New(slog.DiscardHandler)); err == nil || store != nil {
		t.Errorf("start = %v, %v, want an error", store, err)
	}
}

func TestOpenFailsWithoutFolder(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "missing"), slog.New(slog.DiscardHandler))
	if err == nil || store != nil {
		t.Errorf("Open = %v, %v, want an error", store, err)
	}
}

// A folder that can be opened but not written cannot take the temporary folder, and the
// failed start leaves nothing open.
func TestOpenFailsOnAFolderThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if store, err := Open(dir, slog.New(slog.DiscardHandler)); err == nil || store != nil {
		t.Errorf("Open = %v, %v, want an error", store, err)
	}
}

// Save writes the path escaped and the stamp without its device number.
func TestSaveWritesEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	save(t, openStore(t, dir), share, "100%/caf\xe9", 3)
	data, err := os.ReadFile(filepath.Join(dir, share, "100%25", "caf%E9.splicefs_state"))
	if err != nil {
		t.Fatal(err)
	}
	stamp := testStamp(3)
	want := entry{
		Path: "100%25/caf%E9", Size: 3, MtimeNs: stamp.Mtime, CtimeNs: stamp.Ctime, Inode: stamp.Ino,
		Head: testDigests(3).Head, Tail: testDigests(3).Tail,
	}
	if !bytes.Equal(data, want.encode()) {
		t.Errorf("entry file = %q, want %q", data, want.encode())
	}
}

// A failed save is logged once for the whole store, whichever share it was for, and again
// only after a save has worked in between.
func TestKeepWarnsOnceUntilASaveWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := &bytes.Buffer{}
	store, err := Open(dir, slog.New(slog.NewTextHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	block := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	warnings := func() int { return strings.Count(log.String(), "level=WARN") }
	block("a")
	block("b")
	store.Keep("a", "f", testStamp(1), testDigests(1))
	store.Keep("b", "f", testStamp(1), testDigests(1))
	if warnings() != 1 {
		t.Fatalf("%d warnings after failed saves of two shares, want 1:\n%s", warnings(), log)
	}
	store.Keep("c", "f", testStamp(1), testDigests(1))
	if !load(store, "c", "f", 1) {
		t.Fatal("Keep did not save the entry")
	}
	store.Keep("a", "f", testStamp(1), testDigests(1))
	if warnings() != 2 {
		t.Errorf("%d warnings, want a second one after the save that worked:\n%s", warnings(), log)
	}
}

// Reopen finds the digest folder again after it was removed and made again, and makes the
// temporary folder in it, so saves work again without a restart.
func TestReopenFindsTheFolderAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	save(t, store, share, "a", 1)
	if err := errors.Join(os.RemoveAll(dir), os.Mkdir(dir, folderMode)); err != nil {
		t.Fatal(err)
	}
	if err := store.Reopen(); err != nil {
		t.Fatal(err)
	}
	checkTempEmpty(t, dir)
	save(t, store, share, "b", 2)
	if !load(store, share, "b", 2) || load(store, share, "a", 1) {
		t.Error("the store does not use the folder made again")
	}
}

// Reopen on the folder the store has open keeps what is being written in the temporary
// folder, since requests may be writing there, and makes that folder if it is missing.
func TestReopenKeepsTheSameFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := openStore(t, dir)
	writing := filepath.Join(dir, tempFolder, "writing")
	if err := os.WriteFile(writing, nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.Reopen(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(writing); err != nil {
		t.Errorf("Reopen removed a file being written: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, tempFolder)); err != nil {
		t.Fatal(err)
	}
	if err := store.Reopen(); err != nil {
		t.Fatal(err)
	}
	checkTempEmpty(t, dir)
}

func TestReopenFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		breakIt func(t *testing.T, dir string)
	}{
		{"the folder is missing", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file is where the temporary folder goes", func(t *testing.T, dir string) {
			t.Helper()
			temp := filepath.Join(dir, tempFolder)
			if err := errors.Join(os.RemoveAll(temp), os.WriteFile(temp, nil, fileMode)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file is where the folder was", func(t *testing.T, dir string) {
			t.Helper()
			if err := errors.Join(os.RemoveAll(dir), os.WriteFile(dir, nil, fileMode)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "digests")
			if err := os.Mkdir(dir, folderMode); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, dir)
			tt.breakIt(t, dir)
			if err := store.Reopen(); err == nil {
				t.Error("Reopen = nil, want an error")
			}
		})
	}
}
