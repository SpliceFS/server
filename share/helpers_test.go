package share

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// epoch is a fixed modification time far in the past of the test clock.
func epoch() time.Time { return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC) }

// clockTime is the time reported by the injected clock.
func clockTime() time.Time { return time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC) }

// fixture is a temporary directory holding a share root and a sentinel file next to it.
type fixture struct {
	base string
	root string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fix := fixture{base: base, root: filepath.Join(base, "share")}
	if err := os.Mkdir(fix.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fix
}

// write creates a file below the share root with the given modification time.
func (fix fixture) write(t *testing.T, rel, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(fix.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// symlink creates a link below the share root pointing at target.
func (fix fixture) symlink(t *testing.T, target, rel string) {
	t.Helper()
	full := filepath.Join(fix.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// writeOutside creates a file below the folder that holds the share root, so outside
// the share, with the given content.
func (fix fixture) writeOutside(t *testing.T, rel, content string) {
	t.Helper()
	full := filepath.Join(fix.base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// open opens a share over the fixture root with a fixed clock. It takes the options
// by pointer and does not change them.
func (fix fixture) open(t *testing.T, opts *Options) *Share {
	t.Helper()
	options := *opts
	options.Name = "test"
	options.Path = fix.root
	if options.Now == nil {
		options.Now = clockTime
	}
	share, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustClose(t, share) })
	return share
}

// names returns the names of a listing.
func names(infos []Info) []string {
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

// mustClose fails the test when closing closer fails.
func mustClose(tb testing.TB, closer io.Closer) {
	tb.Helper()
	if err := closer.Close(); err != nil {
		tb.Error(err)
	}
}

// requireHidden fails the test unless every operation reports each of the names as not found.
func requireHidden(t *testing.T, share *Share, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := share.Stat(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v", name, err)
		}
		if _, err := share.List(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q) = %v", name, err)
		}
		if _, err := share.OpenFile(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenFile(%q) = %v", name, err)
		}
		if _, _, err := share.HeadDigest(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("HeadDigest(%q) = %v", name, err)
		}
	}
}

// requireListing fails the test unless the directory dir lists exactly the names in want.
func requireListing(t *testing.T, share *Share, dir string, want []string) {
	t.Helper()
	list, err := share.List(dir)
	if err != nil {
		t.Fatalf("List(%q): %v", dir, err)
	}
	if got := names(list); !reflect.DeepEqual(got, want) {
		t.Errorf("List(%q) = %v, want %v", dir, got, want)
	}
}

// mustStat returns the Info of name and fails the test when Stat fails.
func mustStat(t *testing.T, share *Share, name string) Info {
	t.Helper()
	info, err := share.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	return info
}

// mustHeadDigest returns the digest and Info of name and fails the test when HeadDigest fails.
func mustHeadDigest(t *testing.T, share *Share, name string) (string, Info) {
	t.Helper()
	digest, info, err := share.HeadDigest(name)
	if err != nil {
		t.Fatalf("HeadDigest(%q): %v", name, err)
	}
	return digest, info
}

// readFile returns the content of name and fails the test when it cannot be read.
func readFile(t *testing.T, share *Share, name string) string {
	t.Helper()
	file, err := share.OpenFile(name)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", name, err)
	}
	defer mustClose(t, file)
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return string(data)
}

// mustRemove removes the file or empty folder at path and fails the test when that fails.
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// openCounter counts how many files a share opens, so a test can tell a read from a cache hit.
type openCounter struct{ opens int }

// attach makes share report every open to the counter.
func (counter *openCounter) attach(share *Share) {
	share.beforeOpen = func(string) { counter.opens++ }
}
