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

// fuzzContents lists the only file contents that may ever be served from the fuzz share.
// The outside file holds "SECRET-OUTSIDE" and is not in it.
var fuzzContents = map[string]bool{"inside": true, "top": true}

// fuzzShare builds a share root next to files that lie outside it, including links that
// point out of the root by relative path, by absolute path and by way of a directory.
func fuzzShare(f *testing.F, follow bool) *Share {
	f.Helper()
	base, err := filepath.EvalSymlinks(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	root := filepath.Join(base, "share")
	files := map[string]string{
		"share/a/b":         "inside",
		"share/top.txt":     "top",
		"share/.hidden/x":   "inside",
		"secret.txt":        "SECRET-OUTSIDE",
		"outdir/secret.txt": "SECRET-OUTSIDE",
	}
	for rel, content := range files {
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			f.Fatal(err)
		}
	}
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
			f.Fatal(err)
		}
	}
	s, err := Open(Options{Name: "fuzz", Path: root, FollowSymlinks: follow, Exclude: []string{".*"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = s.Close() })
	return s
}

func fuzzNames(f *testing.F) {
	for _, s := range []string{
		"", ".", "a", "a/b", "top.txt", "in", "indir/b", "esc", "escdir/secret.txt",
		"abs", "absdir/secret.txt", "loop", "dangling", "a/up", "a/upup/secret.txt",
		"..", "../secret.txt", "a/../../secret.txt", "a/..", "/secret.txt", "/top.txt",
		`..\secret.txt`, `a\b`, "a\x00b", "a//b", "a/", "./a/b", "a/./b", "//a/b",
		".hidden", ".hidden/x", "a/b/", "a/b/c", "top.txt/", "top.txt/..",
		"%2e%2e/secret.txt", "..%2fsecret.txt", "....//secret.txt", ".../secret.txt",
		strings.Repeat("a/", 5000) + "a", strings.Repeat("a", 5000), strings.Repeat("../", 100),
	} {
		f.Add(s)
	}
}

func FuzzCleanName(f *testing.F) {
	fuzzNames(f)
	f.Fuzz(func(t *testing.T, name string) {
		p, ok := cleanName(name)
		if !ok {
			// A refused name carries no path that a caller could use anyway.
			if p != "" {
				t.Fatalf("cleanName(%q) refused but returned %q", name, p)
			}
			return
		}
		if p == "." {
			return
		}
		// The result is joined to the root, so it must be relative, with no dot or empty
		// segments, and free of NUL bytes that would end a path early in a system call.
		if !fs.ValidPath(p) || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
			t.Fatalf("cleanName(%q) = %q", name, p)
		}
		for segment := range strings.SplitSeq(p, "/") {
			if segment == "" || segment == "." || segment == ".." {
				t.Fatalf("cleanName(%q) = %q has segment %q", name, p, segment)
			}
		}
		// A valid name is used as it is, so the root is not reached by another spelling.
		if p != name {
			t.Fatalf("cleanName(%q) changed the name to %q", name, p)
		}
	})
}

func fuzzShareAccess(f *testing.F, follow bool) {
	s := fuzzShare(f, follow)
	fuzzNames(f)
	f.Fuzz(func(t *testing.T, name string) {
		// Files inside have 3 or 6 bytes and the outside ones 14, so a size says where it is.
		if info, err := s.Stat(name); err == nil && !info.Dir && info.Size != 3 && info.Size != 6 {
			t.Fatalf("Stat(%q) reached the file outside the root", name)
		}
		infos, _ := s.List(name)
		for _, info := range infos {
			// The outside files are named secret.txt, which the share has no entry for.
			if info.Name == "secret.txt" || info.Name == "outdir" {
				t.Fatalf("List(%q) shows %q from outside the root", name, info.Name)
			}
		}
		file, err := s.OpenFile(name)
		if err == nil {
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			// Whatever the name, the bytes served come from a file inside the root.
			if readErr != nil || !fuzzContents[string(data)] {
				t.Fatalf("OpenFile(%q) served %q, %v", name, data, readErr)
			}
		} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrIsDir) {
			t.Fatalf("OpenFile(%q) failed with %v", name, err)
		}
		// A name that cleanName refuses is never found, by any of the operations.
		if _, ok := cleanName(name); !ok {
			if _, _, err := s.HeadDigest(name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("HeadDigest(%q) = %v", name, err)
			}
		}
	})
}

func FuzzShareAccess(f *testing.F)               { fuzzShareAccess(f, false) }
func FuzzShareAccessFollowSymlinks(f *testing.F) { fuzzShareAccess(f, true) }
