package share

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "file.txt", "x", epoch)
	tests := []struct {
		name string
		opts Options
	}{
		{"missing path", Options{Name: "a", Path: filepath.Join(f.base, "nope")}},
		{"file path", Options{Name: "a", Path: filepath.Join(f.root, "file.txt")}},
		{"bad pattern", Options{Name: "a", Path: f.root, Exclude: []string{"[a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := Open(tt.opts)
			if err == nil {
				_ = s.Close()
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), `"a"`) {
				t.Errorf("error %q does not name the share", err)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s, err := Open(Options{Name: "photos", Path: f.root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Name() != "photos" {
		t.Errorf("Name = %q", s.Name())
	}
	if s.now == nil || s.digests.limit != defaultDigestCacheSize {
		t.Errorf("defaults not applied: now=%v limit=%d", s.now != nil, s.digests.limit)
	}
}

func TestStatAndList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	mtime := time.Date(2021, 5, 6, 7, 8, 9, 123456789, time.UTC)
	f.write(t, "b.txt", "hello", mtime)
	f.write(t, "a.txt", "", mtime)
	f.write(t, "sub/c.txt", "ccc", mtime)
	f.write(t, "Z.txt", "z", mtime)
	s := f.open(t, Options{MTimePrecision: time.Second})

	got, err := s.Stat("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		Name:  "b.txt",
		Size:  5,
		MTime: time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC),
	}
	want.ETag = ETag(5, want.MTime)
	if got != want {
		t.Errorf("Stat = %+v, want %+v", got, want)
	}
	if got.MTime.Location() != time.UTC {
		t.Errorf("MTime location = %v", got.MTime.Location())
	}

	dir, err := s.Stat("sub")
	if err != nil || !dir.Dir || dir.Size != 0 || dir.ETag != "" || dir.Name != "sub" {
		t.Errorf("Stat(sub) = %+v, %v", dir, err)
	}

	for _, root := range []string{"", "."} {
		in, err := s.Stat(root)
		if err != nil || !in.Dir || in.Name != "." {
			t.Errorf("Stat(%q) = %+v, %v", root, in, err)
		}
	}

	list, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Z.txt", "a.txt", "b.txt", "sub"}; !reflect.DeepEqual(names(list), want) {
		t.Errorf("List names = %v, want %v", names(list), want)
	}
	sub, err := s.List("sub")
	if err != nil || len(sub) != 1 || sub[0].Name != "c.txt" || sub[0].Size != 3 {
		t.Errorf("List(sub) = %+v, %v", sub, err)
	}
}

func TestFullResolutionKeptWithoutPrecision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	mtime := time.Date(2021, 5, 6, 7, 8, 9, 5000, time.UTC)
	f.write(t, "a", "x", mtime)
	s := f.open(t, Options{})
	got, err := s.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.MTime.Equal(mtime) {
		t.Errorf("MTime = %v, want %v", got.MTime, mtime)
	}
}

func TestETagFormat(t *testing.T) {
	t.Parallel()
	got := ETag(255, time.Unix(4096, 16).UTC())
	if want := `"ff-1000-10"`; got != want {
		t.Errorf("ETag = %s, want %s", got, want)
	}
}

func TestErrorKinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "dir/file.txt", "x", epoch)
	s := f.open(t, Options{})
	long := strings.Repeat("a", 300)

	if _, err := s.List("dir/file.txt"); !errors.Is(err, ErrNotDir) {
		t.Errorf("List(file) = %v, want ErrNotDir", err)
	}
	if _, err := s.OpenFile("dir"); !errors.Is(err, ErrIsDir) {
		t.Errorf("OpenFile(dir) = %v, want ErrIsDir", err)
	}
	if _, _, err := s.HeadDigest("dir"); !errors.Is(err, ErrIsDir) {
		t.Errorf("HeadDigest(dir) = %v, want ErrIsDir", err)
	}
	for _, name := range []string{"missing", "dir/file.txt/x", "dir/missing/x"} {
		if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.List("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.OpenFile("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenFile(missing) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.HeadDigest("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HeadDigest(missing) = %v, want ErrNotFound", err)
	}

	// An over-long name fails with an error that is not "not found".
	_, err := s.Stat(long)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat(long) = %v, want a wrapped error", err)
	}
	if !strings.Contains(err.Error(), `share "test"`) || !strings.Contains(err.Error(), long) {
		t.Errorf("error %q lacks share name or path", err)
	}
}

func TestInvalidNames(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "a/b", "inside", epoch)
	s := f.open(t, Options{})
	names := []string{
		"../secret.txt",
		"..",
		"a/../../secret.txt",
		"a/..",
		"/secret.txt",
		"/" + filepath.ToSlash(f.base) + "/secret.txt",
		`..\secret.txt`,
		`a\b`,
		"a\x00b",
		"a//b",
		"a/",
		"./a/b",
		"a/./b",
		"//a/b",
	}
	for _, name := range names {
		if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v", name, err)
		}
		if _, err := s.List(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q) = %v", name, err)
		}
		if _, err := s.OpenFile(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenFile(%q) = %v", name, err)
		}
		if _, _, err := s.HeadDigest(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("HeadDigest(%q) = %v", name, err)
		}
	}
}

func TestSymlinksOutsideAreInvisible(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "nofollow", true: "follow"}[follow], func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.write(t, "real.txt", "inside", epoch)
			outDir := filepath.Join(f.base, "other")
			if err := os.Mkdir(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outDir, "data.txt"), []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			f.symlink(t, filepath.Join(f.base, "secret.txt"), "abs-file")
			f.symlink(t, "../secret.txt", "rel-file")
			f.symlink(t, outDir, "abs-dir")
			f.symlink(t, "../other", "rel-dir")
			f.symlink(t, "../other/data.txt", "sub/deep-file")
			s := f.open(t, Options{FollowSymlinks: follow})

			list, err := s.List("")
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"real.txt", "sub"}; !reflect.DeepEqual(names(list), want) {
				t.Errorf("List = %v, want %v", names(list), want)
			}
			if sub, err := s.List("sub"); err != nil || len(sub) != 0 {
				t.Errorf("List(sub) = %v, %v", sub, err)
			}
			for _, name := range []string{
				"abs-file", "rel-file", "abs-dir", "rel-dir",
				"abs-dir/data.txt", "rel-dir/data.txt", "sub/deep-file",
			} {
				if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
					t.Errorf("Stat(%q) = %v", name, err)
				}
				if _, err := s.OpenFile(name); !errors.Is(err, ErrNotFound) {
					t.Errorf("OpenFile(%q) = %v", name, err)
				}
				if _, _, err := s.HeadDigest(name); !errors.Is(err, ErrNotFound) {
					t.Errorf("HeadDigest(%q) = %v", name, err)
				}
				if _, err := s.List(name); !errors.Is(err, ErrNotFound) {
					t.Errorf("List(%q) = %v", name, err)
				}
			}
		})
	}
}

func TestSymlinksInside(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "real/file.txt", "inside", epoch)
	f.symlink(t, "real/file.txt", "link-file")
	f.symlink(t, "real", "link-dir")
	f.symlink(t, "link-file", "link-chain")
	f.symlink(t, "missing", "dangling")
	f.symlink(t, "loop", "loop")
	f.symlink(t, "/etc", "link-abs")

	t.Run("hidden when not followed", func(t *testing.T) {
		t.Parallel()
		s := f.open(t, Options{})
		list, err := s.List("")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"real"}; !reflect.DeepEqual(names(list), want) {
			t.Errorf("List = %v, want %v", names(list), want)
		}
		for _, name := range []string{"link-file", "link-dir", "link-dir/file.txt", "link-chain"} {
			if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
				t.Errorf("Stat(%q) = %v", name, err)
			}
			if _, err := s.OpenFile(name); !errors.Is(err, ErrNotFound) {
				t.Errorf("OpenFile(%q) = %v", name, err)
			}
		}
	})

	t.Run("followed", func(t *testing.T) {
		t.Parallel()
		s := f.open(t, Options{FollowSymlinks: true})
		list, err := s.List("")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"link-chain", "link-dir", "link-file", "real"}; !reflect.DeepEqual(names(list), want) {
			t.Errorf("List = %v, want %v", names(list), want)
		}
		for _, name := range []string{"link-file", "link-dir/file.txt", "link-chain"} {
			in, err := s.Stat(name)
			if err != nil || in.Dir || in.Size != 6 {
				t.Errorf("Stat(%q) = %+v, %v", name, in, err)
			}
			file, err := s.OpenFile(name)
			if err != nil {
				t.Fatalf("OpenFile(%q): %v", name, err)
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			if string(data) != "inside" {
				t.Errorf("OpenFile(%q) read %q", name, data)
			}
		}
		if in, err := s.Stat("link-dir"); err != nil || !in.Dir || in.Name != "link-dir" {
			t.Errorf("Stat(link-dir) = %+v, %v", in, err)
		}
		sub, err := s.List("link-dir")
		if err != nil || len(sub) != 1 || sub[0].Name != "file.txt" {
			t.Errorf("List(link-dir) = %+v, %v", sub, err)
		}
		for _, name := range []string{"dangling", "loop", "link-abs", "link-abs/passwd"} {
			if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
				t.Errorf("Stat(%q) = %v", name, err)
			}
		}
	})
}

func TestExclude(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, ".hidden", "x", epoch)
	f.write(t, "keep.txt", "x", epoch)
	f.write(t, "tmp.part", "x", epoch)
	f.write(t, ".git/config", "x", epoch)
	f.write(t, "docs/.cache/blob", "x", epoch)
	f.write(t, "docs/readme", "x", epoch)
	s := f.open(t, Options{Exclude: []string{".*", "*.part"}})

	list, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs", "keep.txt"}; !reflect.DeepEqual(names(list), want) {
		t.Errorf("List = %v, want %v", names(list), want)
	}
	docs, err := s.List("docs")
	if err != nil || !reflect.DeepEqual(names(docs), []string{"readme"}) {
		t.Errorf("List(docs) = %v, %v", names(docs), err)
	}
	for _, name := range []string{".hidden", "tmp.part", ".git", ".git/config", "docs/.cache", "docs/.cache/blob"} {
		if _, err := s.Stat(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v", name, err)
		}
		if _, err := s.OpenFile(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenFile(%q) = %v", name, err)
		}
		if _, err := s.List(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q) = %v", name, err)
		}
	}
}

func TestMinAge(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	now := clockTime
	f.write(t, "old.txt", "x", now.Add(-time.Hour))
	f.write(t, "edge.txt", "x", now.Add(-10*time.Minute))
	f.write(t, "new.txt", "x", now.Add(-time.Minute))
	f.write(t, "newdir/old.txt", "x", now.Add(-time.Hour))
	if err := os.Chtimes(filepath.Join(f.root, "newdir"), now, now); err != nil {
		t.Fatal(err)
	}
	clock := now
	s := f.open(t, Options{MinAge: 10 * time.Minute, Now: func() time.Time { return clock }})

	list, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"edge.txt", "newdir", "old.txt"}; !reflect.DeepEqual(names(list), want) {
		t.Errorf("List = %v, want %v", names(list), want)
	}
	if _, err := s.Stat("new.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(new.txt) = %v", err)
	}
	if _, err := s.OpenFile("new.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenFile(new.txt) = %v", err)
	}
	if _, _, err := s.HeadDigest("new.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HeadDigest(new.txt) = %v", err)
	}
	if _, err := s.Stat("newdir"); err != nil {
		t.Errorf("Stat(newdir) = %v, directories are never hidden by age", err)
	}

	clock = now.Add(10 * time.Minute)
	if _, err := s.Stat("new.txt"); err != nil {
		t.Errorf("Stat(new.txt) after the clock advanced = %v", err)
	}
}

func TestSameFileSameIdentity(t *testing.T) {
	t.Parallel()
	mtime := time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)
	a, b := newFixture(t), newFixture(t)
	a.write(t, "x/f.bin", "same content", mtime)
	b.write(t, "x/f.bin", "same content", mtime.Add(400*time.Millisecond))
	sa := a.open(t, Options{MTimePrecision: time.Second})
	sb := b.open(t, Options{MTimePrecision: time.Second})

	ia, err := sa.Stat("x/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	ib, err := sb.Stat("x/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ia != ib {
		t.Errorf("infos differ: %+v and %+v", ia, ib)
	}
	da, _, err := sa.HeadDigest("x/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := sb.HeadDigest("x/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("digests differ: %s and %s", da, db)
	}
	// SHA-256 of "same content".
	if len(da) != 64 || da != strings.ToLower(da) {
		t.Errorf("digest %q is not lowercase hex SHA-256", da)
	}
}

func TestOpenFileReportsTheFileActuallyOpened(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "f", "old", epoch)
	s := f.open(t, Options{MTimePrecision: time.Second})
	before, err := s.Stat("f")
	if err != nil {
		t.Fatal(err)
	}
	newTime := epoch.Add(time.Hour)
	s.beforeOpen = func(string) {
		f.write(t, "f.new", "much longer", newTime)
		if err := os.Rename(filepath.Join(f.root, "f.new"), filepath.Join(f.root, "f")); err != nil {
			t.Error(err)
		}
	}
	file, err := s.OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	got := file.Info()
	if got.Size != 11 || !got.MTime.Equal(newTime) || got.ETag != ETag(11, newTime) || got == before {
		t.Errorf("Info = %+v, before = %+v", got, before)
	}
}

func TestOpenFileRacesAreRechecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, full string)
		want   error
	}{
		{"deleted", func(t *testing.T, full string) {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
		}, ErrNotFound},
		{"replaced by directory", func(t *testing.T, full string) {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(full, 0o755); err != nil {
				t.Fatal(err)
			}
		}, ErrIsDir},
		{"replaced by pipe", func(t *testing.T, full string) {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			if err := mkfifo(full); err != nil {
				t.Skipf("cannot create a named pipe: %v", err)
			}
		}, ErrNotFound},
		{"replaced by fresh file", func(t *testing.T, full string) {
			if err := os.Chtimes(full, clockTime, clockTime); err != nil {
				t.Fatal(err)
			}
		}, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.write(t, "f", "x", epoch)
			s := f.open(t, Options{MinAge: time.Hour})
			s.beforeOpen = func(string) { tt.mutate(t, filepath.Join(f.root, "f")) }
			if file, err := s.OpenFile("f"); !errors.Is(err, tt.want) {
				if err == nil {
					_ = file.Close()
				}
				t.Errorf("OpenFile = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestListOpenRace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "d/x", "x", epoch)
	s := f.open(t, Options{})
	s.beforeOpen = func(string) {
		if err := os.RemoveAll(filepath.Join(f.root, "d")); err != nil {
			t.Error(err)
		}
	}
	if _, err := s.List("d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List = %v, want ErrNotFound", err)
	}
}

func TestHeadDigestCache(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "f", "first", epoch)
	s := f.open(t, Options{MTimePrecision: time.Second})
	opens := 0
	s.beforeOpen = func(string) { opens++ }

	d1, info1, err := s.HeadDigest("f")
	if err != nil {
		t.Fatal(err)
	}
	d2, info2, err := s.HeadDigest("f")
	if err != nil {
		t.Fatal(err)
	}
	if opens != 1 || d1 != d2 || info1 != info2 {
		t.Errorf("opens = %d, d1 = %s, d2 = %s", opens, d1, d2)
	}

	// Same size, new modification time: a different version.
	f.write(t, "f", "other", epoch.Add(time.Minute))
	d3, info3, err := s.HeadDigest("f")
	if err != nil {
		t.Fatal(err)
	}
	if opens != 2 || d3 == d1 || info3.ETag == info1.ETag {
		t.Errorf("opens = %d, d3 = %s", opens, d3)
	}

	// New size, same modification time.
	f.write(t, "f", "longer content", epoch.Add(time.Minute))
	d4, _, err := s.HeadDigest("f")
	if err != nil {
		t.Fatal(err)
	}
	if opens != 3 || d4 == d3 {
		t.Errorf("opens = %d, d4 = %s", opens, d4)
	}
}

func TestHeadDigestCoversOnlyTheHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	head := strings.Repeat("h", 64<<10)
	f.write(t, "a", head+"tail one", epoch)
	f.write(t, "b", head+"tail two", epoch)
	f.write(t, "short", "abc", epoch)
	s := f.open(t, Options{})
	da, _, err := s.HeadDigest("a")
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := s.HeadDigest("b")
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("digests of equal heads differ")
	}
	ds, _, err := s.HeadDigest("short")
	if err != nil {
		t.Fatal(err)
	}
	// SHA-256 of "abc".
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; ds != want {
		t.Errorf("digest = %s, want %s", ds, want)
	}
}

func TestHeadDigestCacheEvicts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.write(t, n, n, epoch)
	}
	s := f.open(t, Options{DigestCacheSize: 2})
	opens := 0
	s.beforeOpen = func(string) { opens++ }
	digest := func(n string) {
		t.Helper()
		if _, _, err := s.HeadDigest(n); err != nil {
			t.Fatal(err)
		}
	}
	digest("a")
	digest("b")
	digest("c") // evicts a
	if opens != 3 {
		t.Fatalf("opens = %d, want 3", opens)
	}
	digest("c")
	digest("b")
	if opens != 3 {
		t.Errorf("cached entries were read again: opens = %d", opens)
	}
	digest("a")
	if opens != 4 {
		t.Errorf("evicted entry was not read again: opens = %d", opens)
	}
}

func TestHeadDigestOpenFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "f", "x", epoch)
	s := f.open(t, Options{})
	s.beforeOpen = func(string) { _ = os.Remove(filepath.Join(f.root, "f")) }
	if _, _, err := s.HeadDigest("f"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HeadDigest = %v, want ErrNotFound", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestHeadDigestReadError(t *testing.T) {
	t.Parallel()
	if _, err := headDigest(failingReader{}); err == nil {
		t.Error("headDigest succeeded on a failing reader")
	}
	f := newFixture(t)
	f.write(t, "f", "x", epoch)
	s := f.open(t, Options{})
	s.headDigest = func(io.Reader) (string, error) { return headDigest(failingReader{}) }
	_, _, err := s.HeadDigest("f")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `share "test"`) {
		t.Errorf("HeadDigest = %v", err)
	}
}

func TestFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "f", "0123456789", epoch)
	s := f.open(t, Options{})
	file, err := s.OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	var _ io.ReadSeeker = file
	if file.Info().Size != 10 {
		t.Errorf("Info = %+v", file.Info())
	}
	if _, err := file.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(file, buf); err != nil || string(buf) != "456" {
		t.Errorf("read %q, %v", buf, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Read(buf); err == nil {
		t.Error("Read after Close succeeded")
	}
}

type fakeDir struct {
	names []string
	err   error
}

func (d fakeDir) Readdirnames(int) ([]string, error) { return d.names, d.err }

func TestCollect(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "d/real", "x", epoch)
	s := f.open(t, Options{})

	if _, err := s.collect("d", fakeDir{err: errors.New("read failed")}); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("collect with a read error = %v", err)
	}
	// An entry that vanished after the directory was read is skipped.
	got, err := s.collect("d", fakeDir{names: []string{"vanished", "real"}})
	if err != nil || !reflect.DeepEqual(names(got), []string{"real"}) {
		t.Errorf("collect = %v, %v", names(got), err)
	}
	// An entry that cannot be examined fails the listing.
	if _, err := s.collect("d", fakeDir{names: []string{strings.Repeat("a", 300)}}); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("collect with an unreadable entry = %v", err)
	}
}

type fakeHandle struct{ err error }

func (h fakeHandle) Stat() (os.FileInfo, error) { return nil, h.err }

func TestOpenInfoStatError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.open(t, Options{})
	err := func() error { _, err := s.openInfo("f", fakeHandle{err: errors.New("stat failed")}); return err }()
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("openInfo = %v", err)
	}
}

func TestAbsent(t *testing.T) {
	t.Parallel()
	pe := func(err error) error { return &fs.PathError{Op: "x", Path: "p", Err: err} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"not exist", pe(fs.ErrNotExist), true},
		{"not a directory", pe(errSyscall("ENOTDIR")), true},
		{"loop", pe(errSyscall("ELOOP")), true},
		{"escape", pe(errors.New("path escapes from parent")), true},
		{"permission", pe(fs.ErrPermission), false},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		if got := absent(tt.err); got != tt.want {
			t.Errorf("%s: absent = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSpace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.open(t, Options{})
	sp, err := s.Space()
	if err != nil {
		t.Fatal(err)
	}
	if sp.Total == 0 || sp.Free > sp.Total {
		t.Errorf("Space = %+v", sp)
	}
}

func TestSpaceError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.open(t, Options{})
	s.space = func(string) (sp wireSpace, err error) { return sp, errors.New("statfs failed") }
	if _, err := s.Space(); err == nil || !strings.Contains(err.Error(), `share "test"`) {
		t.Errorf("Space = %v", err)
	}
	if _, err := statSpace(filepath.Join(f.base, "missing")); err == nil {
		t.Error("statSpace on a missing path succeeded")
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c", "d/e"} {
		f.write(t, n, n, epoch)
	}
	s := f.open(t, Options{MTimePrecision: time.Second, DigestCacheSize: 2})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				for _, n := range []string{"a", "b", "c", "d/e"} {
					if _, _, err := s.HeadDigest(n); err != nil {
						t.Error(err)
					}
					if _, err := s.Stat(n); err != nil {
						t.Error(err)
					}
				}
				if _, err := s.List(""); err != nil {
					t.Error(err)
				}
				if _, err := s.Space(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestBackslashIsAnOrdinaryCharacter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, `odd\name`, "content", epoch)
	s := f.open(t, Options{})
	infos, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != `odd\name` {
		t.Fatalf("List = %+v, want the one file with a backslash in its name", infos)
	}
	file, err := s.OpenFile(`odd\name`)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "content" {
		t.Errorf("read = %q, %v", data, err)
	}
}

func TestListHidesNamesThatAreNotUTF8(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(t, "d/real", "x", epoch)
	s := f.open(t, Options{})
	got, err := s.collect("d", fakeDir{names: []string{"bad\xff", "real"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("collect = %+v, want only the valid name", got)
	}
}
