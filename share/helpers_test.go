package share

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// epoch is a fixed modification time far in the past of the test clock.
var epoch = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// clockTime is the time reported by the injected clock.
var clockTime = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

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
	f := fixture{base: base, root: filepath.Join(base, "share")}
	if err := os.Mkdir(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// write creates a file below the share root with the given modification time.
func (f fixture) write(t *testing.T, rel, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(f.root, filepath.FromSlash(rel))
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
func (f fixture) symlink(t *testing.T, target, rel string) {
	t.Helper()
	full := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// open opens a share over the fixture root with a fixed clock.
func (f fixture) open(t *testing.T, opts Options) *Share {
	t.Helper()
	opts.Name = "test"
	opts.Path = f.root
	if opts.Now == nil {
		opts.Now = func() time.Time { return clockTime }
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// names returns the names of a listing.
func names(infos []Info) []string {
	out := make([]string, len(infos))
	for i, in := range infos {
		out[i] = in.Name
	}
	return out
}
