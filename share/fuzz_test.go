package share

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The contents of the files in the fuzz share. Only the first two may ever be served;
// the outside file must never be.
const (
	contentInside  = "inside"
	contentTop     = "top"
	contentOutside = "SECRET-OUTSIDE"
)

// writeFuzzFiles creates the files of the fuzz share and the files next to it, below base.
func writeFuzzFiles(tb testing.TB, base string) {
	tb.Helper()
	files := map[string]string{
		"share/a/b":         contentInside,
		"share/top.txt":     contentTop,
		"share/.hidden/x":   contentInside,
		"secret.txt":        contentOutside,
		"outdir/secret.txt": contentOutside,
	}
	for rel, content := range files {
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

// linkFuzzShare creates links in the share root that point out of it by relative path, by
// absolute path and by way of a directory, and links that stay inside, dangle or loop.
func linkFuzzShare(tb testing.TB, base, root string) {
	tb.Helper()
	links := map[string]string{
		"esc":      "../secret.txt",
		"escdir":   "../outdir",
		"abs":      filepath.Join(base, "secret.txt"),
		"absdir":   base,
		"in":       "a/b",
		"indir":    "a",
		"loop":     "loop",
		"dangling": "nowhere",
		"a/up":     "../../secret.txt",
		"a/upup":   "../../../" + filepath.Base(base),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			tb.Fatal(err)
		}
	}
}

// fuzzShare builds a share root next to files that lie outside it, with links that
// try to reach them.
func fuzzShare(f *testing.F, follow bool) *Share {
	f.Helper()
	base, err := filepath.EvalSymlinks(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	root := filepath.Join(base, "share")
	writeFuzzFiles(f, base)
	linkFuzzShare(f, base, root)
	share, err := Open(Options{Name: "fuzz", Path: root, FollowSymlinks: follow, Exclude: []string{".*"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { mustClose(f, share) })
	return share
}

func fuzzNames(f *testing.F) {
	f.Helper()
	for _, name := range []string{
		"", ".", "a", "a/b", "top.txt", "in", "indir/b", "esc", "escdir/secret.txt",
		"abs", "absdir/secret.txt", "loop", "dangling", "a/up", "a/upup/secret.txt",
		"..", "../secret.txt", "a/../../secret.txt", "a/..", "/secret.txt", "/top.txt",
		`..\secret.txt`, `a\b`, "a\x00b", "a//b", "a/", "./a/b", "a/./b", "//a/b",
		".hidden", ".hidden/x", "a/b/", "a/b/c", "top.txt/", "top.txt/..",
		"%2e%2e/secret.txt", "..%2fsecret.txt", "....//secret.txt", ".../secret.txt",
		strings.Repeat("a/", 5000) + "a", strings.Repeat("a", 5000), strings.Repeat("../", 100),
	} {
		f.Add(name)
	}
}

// requireUsablePath fails the test unless cleaned, the result of cleanName for name, can
// be joined to the share root without leaving it.
func requireUsablePath(t *testing.T, name, cleaned string) {
	t.Helper()
	// The result is joined to the root, so it must be relative, with no dot or empty
	// segments, and free of NUL bytes that would end a path early in a system call.
	if !fs.ValidPath(cleaned) || strings.HasPrefix(cleaned, "/") || strings.ContainsRune(cleaned, 0) {
		t.Fatalf("cleanName(%q) = %q", name, cleaned)
	}
	for segment := range strings.SplitSeq(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			t.Fatalf("cleanName(%q) = %q has segment %q", name, cleaned, segment)
		}
	}
	// A valid name is used as it is, so the root is not reached by another spelling.
	if cleaned != name {
		t.Fatalf("cleanName(%q) changed the name to %q", name, cleaned)
	}
}

func FuzzCleanName(f *testing.F) {
	fuzzNames(f)
	f.Fuzz(func(t *testing.T, name string) {
		cleaned, ok := cleanName(name)
		if !ok {
			// A refused name carries no path that a caller could use anyway.
			if cleaned != "" {
				t.Fatalf("cleanName(%q) refused but returned %q", name, cleaned)
			}
			return
		}
		if cleaned != "." {
			requireUsablePath(t, name, cleaned)
		}
	})
}

// requireStatInside fails the test when Stat reaches a file outside the share root.
func requireStatInside(t *testing.T, share *Share, name string) {
	t.Helper()
	// Files inside have 3 or 6 bytes and the outside ones 14, so a size says where it is.
	if info, err := share.Stat(name); err == nil && !info.Dir && info.Size != 3 && info.Size != 6 {
		t.Fatalf("Stat(%q) reached the file outside the root", name)
	}
}

// requireListInside fails the test when List shows an entry from outside the share root.
func requireListInside(t *testing.T, share *Share, name string) {
	t.Helper()
	infos, err := share.List(name)
	if err != nil {
		return
	}
	for _, info := range infos {
		// The outside files are named secret.txt, which the share has no entry for.
		if info.Name == "secret.txt" || info.Name == "outdir" {
			t.Fatalf("List(%q) shows %q from outside the root", name, info.Name)
		}
	}
}

// requireOpenInside fails the test when OpenFile serves bytes from outside the share root.
func requireOpenInside(t *testing.T, share *Share, name string) {
	t.Helper()
	file, err := share.OpenFile(name)
	if err != nil {
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrIsDir) {
			t.Fatalf("OpenFile(%q) failed with %v", name, err)
		}
		return
	}
	data, readErr := io.ReadAll(file)
	mustClose(t, file)
	// Whatever the name, the bytes served come from a file inside the root.
	if readErr != nil || (string(data) != contentInside && string(data) != contentTop) {
		t.Fatalf("OpenFile(%q) served %q, %v", name, data, readErr)
	}
}

// requireRefusedNameNotFound fails the test when a name that cleanName refuses is found.
func requireRefusedNameNotFound(t *testing.T, share *Share, name string) {
	t.Helper()
	if _, ok := cleanName(name); ok {
		return
	}
	if _, _, err := share.HeadDigest(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HeadDigest(%q) = %v", name, err)
	}
}

func runShareAccessFuzz(f *testing.F, follow bool) {
	f.Helper()
	share := fuzzShare(f, follow)
	fuzzNames(f)
	f.Fuzz(func(t *testing.T, name string) {
		requireStatInside(t, share, name)
		requireListInside(t, share, name)
		requireOpenInside(t, share, name)
		requireRefusedNameNotFound(t, share, name)
	})
}

func FuzzShareAccess(f *testing.F)               { runShareAccessFuzz(f, false) }
func FuzzShareAccessFollowSymlinks(f *testing.F) { runShareAccessFuzz(f, true) }
