package share

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "file.txt", "x", epoch())
	tests := []struct {
		name string
		opts Options
	}{
		{"missing path", Options{Name: "a", Path: filepath.Join(fix.base, "nope")}},
		{"file path", Options{Name: "a", Path: filepath.Join(fix.root, "file.txt")}},
		{"bad pattern", Options{Name: "a", Path: fix.root, Exclude: []string{"[a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			share, err := Open(tt.opts)
			if err == nil {
				mustClose(t, share)
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
	fix := newFixture(t)
	share, err := Open(Options{Name: "photos", Path: fix.root})
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, share)
	if share.Name() != "photos" {
		t.Errorf("Name = %q", share.Name())
	}
	if share.now == nil || share.digests.limit != defaultDigestCacheSize {
		t.Errorf("defaults not applied: now=%v limit=%d", share.now != nil, share.digests.limit)
	}
}

// listingShare opens a share over a few files whose modification time has a fraction
// of a second, with a precision of one second.
func listingShare(t *testing.T) *Share {
	t.Helper()
	fix := newFixture(t)
	mtime := time.Date(2021, 5, 6, 7, 8, 9, 123456789, time.UTC)
	fix.write(t, "b.txt", "hello", mtime)
	fix.write(t, "a.txt", "", mtime)
	fix.write(t, "sub/c.txt", "ccc", mtime)
	fix.write(t, "Z.txt", "z", mtime)
	return fix.open(t, &Options{MTimePrecision: time.Second})
}

func TestStatFile(t *testing.T) {
	t.Parallel()
	got := mustStat(t, listingShare(t), "b.txt")
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
}

func TestStatDirectories(t *testing.T) {
	t.Parallel()
	share := listingShare(t)
	dir := mustStat(t, share, "sub")
	if !dir.Dir || dir.Size != 0 || dir.ETag != "" || dir.Name != "sub" {
		t.Errorf("Stat(sub) = %+v", dir)
	}
	for _, root := range []string{"", "."} {
		info := mustStat(t, share, root)
		if !info.Dir || info.Name != "." {
			t.Errorf("Stat(%q) = %+v", root, info)
		}
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	share := listingShare(t)
	requireListing(t, share, "", []string{"Z.txt", "a.txt", "b.txt", "sub"})
	sub, err := share.List("sub")
	if err != nil || len(sub) != 1 || sub[0].Name != "c.txt" || sub[0].Size != 3 {
		t.Errorf("List(sub) = %+v, %v", sub, err)
	}
}

func TestFullResolutionKeptWithoutPrecision(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	mtime := time.Date(2021, 5, 6, 7, 8, 9, 5000, time.UTC)
	fix.write(t, "a", "x", mtime)
	got := mustStat(t, fix.open(t, &Options{}), "a")
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

func TestErrorKindsForTheWrongType(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "dir/file.txt", "x", epoch())
	share := fix.open(t, &Options{})

	if _, err := share.List("dir/file.txt"); !errors.Is(err, ErrNotDir) {
		t.Errorf("List(file) = %v, want ErrNotDir", err)
	}
	if _, err := share.OpenFile("dir"); !errors.Is(err, ErrIsDir) {
		t.Errorf("OpenFile(dir) = %v, want ErrIsDir", err)
	}
	if _, _, err := share.Digests("dir"); !errors.Is(err, ErrIsDir) {
		t.Errorf("Digests(dir) = %v, want ErrIsDir", err)
	}
}

func TestErrorKindsForMissingNames(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "dir/file.txt", "x", epoch())
	share := fix.open(t, &Options{})

	requireHidden(t, share, "missing")
	for _, name := range []string{"dir/file.txt/x", "dir/missing/x"} {
		if _, err := share.Stat(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", name, err)
		}
	}
}

func TestNamesLongerThanTheFileSystemAllowsAreMissing(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "dir/file.txt", "x", epoch())
	share := fix.open(t, &Options{})
	long := strings.Repeat("a", 300)

	// Such a name cannot exist, whatever way it is asked for.
	requireHidden(t, share, long, "dir/"+long, strings.Repeat("a/", 5000)+"a")
}

func TestInvalidNames(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "a/b", "inside", epoch())
	share := fix.open(t, &Options{})
	requireHidden(t, share,
		"../secret.txt",
		"..",
		"a/../../secret.txt",
		"a/..",
		"/secret.txt",
		"/"+filepath.ToSlash(fix.base)+"/secret.txt",
		`..\secret.txt`,
		`a\b`,
		"a\x00b",
		"a//b",
		"a/",
		"./a/b",
		"a/./b",
		"//a/b",
	)
}

// outsideLinksShare opens a share with one real file, and with links that point out of
// the share root by relative path, by absolute path, to a file and to a folder.
func outsideLinksShare(t *testing.T, follow bool) *Share {
	t.Helper()
	fix := newFixture(t)
	fix.write(t, "real.txt", "inside", epoch())
	fix.writeOutside(t, "other/data.txt", "outside")
	fix.symlink(t, filepath.Join(fix.base, "secret.txt"), "abs-file")
	fix.symlink(t, "../secret.txt", "rel-file")
	fix.symlink(t, filepath.Join(fix.base, "other"), "abs-dir")
	fix.symlink(t, "../other", "rel-dir")
	fix.symlink(t, "../other/data.txt", "sub/deep-file")
	return fix.open(t, &Options{FollowSymlinks: follow})
}

func TestSymlinksOutsideAreInvisible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		follow bool
	}{
		{"nofollow", false},
		{"follow", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			share := outsideLinksShare(t, tt.follow)
			requireListing(t, share, "", []string{"real.txt", "sub"})
			requireListing(t, share, "sub", []string{})
			requireHidden(t, share,
				"abs-file", "rel-file", "abs-dir", "rel-dir",
				"abs-dir/data.txt", "rel-dir/data.txt", "sub/deep-file",
			)
		})
	}
}

// insideLinksFixture holds a real file, links that stay inside the share root, and
// links that dangle, loop or leave the root.
func insideLinksFixture(t *testing.T) fixture {
	t.Helper()
	fix := newFixture(t)
	fix.write(t, "real/file.txt", "inside", epoch())
	fix.symlink(t, "real/file.txt", "link-file")
	fix.symlink(t, "real", "link-dir")
	fix.symlink(t, "link-file", "link-chain")
	fix.symlink(t, "missing", "dangling")
	fix.symlink(t, "loop", "loop")
	fix.symlink(t, "/etc", "link-abs")
	return fix
}

func TestSymlinksInsideAreHiddenWhenNotFollowed(t *testing.T) {
	t.Parallel()
	share := insideLinksFixture(t).open(t, &Options{})
	requireListing(t, share, "", []string{"real"})
	requireHidden(t, share, "link-file", "link-dir", "link-dir/file.txt", "link-chain")
}

func TestSymlinksInsideAreServedWhenFollowed(t *testing.T) {
	t.Parallel()
	share := insideLinksFixture(t).open(t, &Options{FollowSymlinks: true})
	requireListing(t, share, "", []string{"link-chain", "link-dir", "link-file", "real"})
	for _, name := range []string{"link-file", "link-dir/file.txt", "link-chain"} {
		if info := mustStat(t, share, name); info.Dir || info.Size != 6 {
			t.Errorf("Stat(%q) = %+v", name, info)
		}
		if got := readFile(t, share, name); got != "inside" {
			t.Errorf("OpenFile(%q) read %q", name, got)
		}
	}
	if info := mustStat(t, share, "link-dir"); !info.Dir || info.Name != "link-dir" {
		t.Errorf("Stat(link-dir) = %+v", info)
	}
	requireListing(t, share, "link-dir", []string{"file.txt"})
}

func TestSymlinksThatCannotBeResolvedInsideAreHiddenWhenFollowed(t *testing.T) {
	t.Parallel()
	share := insideLinksFixture(t).open(t, &Options{FollowSymlinks: true})
	requireHidden(t, share, "dangling", "loop", "link-abs", "link-abs/passwd")
}

func TestExclude(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, ".hidden", "x", epoch())
	fix.write(t, "keep.txt", "x", epoch())
	fix.write(t, "tmp.part", "x", epoch())
	fix.write(t, ".git/config", "x", epoch())
	fix.write(t, "docs/.cache/blob", "x", epoch())
	fix.write(t, "docs/readme", "x", epoch())
	share := fix.open(t, &Options{Exclude: []string{".*", "*.part"}})

	requireListing(t, share, "", []string{"docs", "keep.txt"})
	requireListing(t, share, "docs", []string{"readme"})
	requireHidden(t, share, ".hidden", "tmp.part", ".git", ".git/config", "docs/.cache", "docs/.cache/blob")
}

func TestMinAge(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	now := clockTime()
	fix.write(t, "old.txt", "x", now.Add(-time.Hour))
	fix.write(t, "edge.txt", "x", now.Add(-10*time.Minute))
	fix.write(t, "new.txt", "x", now.Add(-time.Minute))
	fix.write(t, "newdir/old.txt", "x", now.Add(-time.Hour))
	if err := os.Chtimes(filepath.Join(fix.root, "newdir"), now, now); err != nil {
		t.Fatal(err)
	}
	clock := now
	share := fix.open(t, &Options{MinAge: 10 * time.Minute, Now: func() time.Time { return clock }})

	requireListing(t, share, "", []string{"edge.txt", "newdir", "old.txt"})
	requireHidden(t, share, "new.txt")
	// Directories are never hidden by age.
	mustStat(t, share, "newdir")

	clock = now.Add(10 * time.Minute)
	// The clock has advanced, so the file is old enough now.
	mustStat(t, share, "new.txt")
}

func TestSameFileSameIdentity(t *testing.T) {
	t.Parallel()
	mtime := time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)
	fixA, fixB := newFixture(t), newFixture(t)
	fixA.write(t, "x/f.bin", "same content", mtime)
	fixB.write(t, "x/f.bin", "same content", mtime.Add(400*time.Millisecond))
	shareA := fixA.open(t, &Options{MTimePrecision: time.Second})
	shareB := fixB.open(t, &Options{MTimePrecision: time.Second})

	infoA := mustStat(t, shareA, "x/f.bin")
	infoB := mustStat(t, shareB, "x/f.bin")
	if infoA != infoB {
		t.Errorf("infos differ: %+v and %+v", infoA, infoB)
	}
	digestA, _ := mustDigests(t, shareA, "x/f.bin")
	digestB, _ := mustDigests(t, shareB, "x/f.bin")
	if digestA != digestB {
		t.Errorf("digests differ: %+v and %+v", digestA, digestB)
	}
	for _, digest := range []string{digestA.Head, digestA.Tail} {
		if len(digest) != 64 || digest != strings.ToLower(digest) {
			t.Errorf("digest %q is not lowercase hex SHA-256", digest)
		}
	}
}

func TestOpenFileReportsTheFileActuallyOpened(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "f", "old", epoch())
	share := fix.open(t, &Options{MTimePrecision: time.Second})
	before := mustStat(t, share, "f")
	newTime := epoch().Add(time.Hour)
	share.beforeOpen = func(string) {
		fix.write(t, "f.new", "much longer", newTime)
		if err := os.Rename(filepath.Join(fix.root, "f.new"), filepath.Join(fix.root, "f")); err != nil {
			t.Error(err)
		}
	}
	file, err := share.OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, file)
	got := file.Info()
	if got.Size != 11 || !got.MTime.Equal(newTime) || got.ETag != ETag(11, newTime) || got == before {
		t.Errorf("Info = %+v, before = %+v", got, before)
	}
}

// replaceWithDirectory swaps the file full for a folder of the same name.
func replaceWithDirectory(t *testing.T, full string) {
	t.Helper()
	mustRemove(t, full)
	if err := os.Mkdir(full, 0o755); err != nil {
		t.Fatal(err)
	}
}

// replaceWithPipe swaps the file full for a named pipe of the same name.
func replaceWithPipe(t *testing.T, full string) {
	t.Helper()
	mustRemove(t, full)
	if err := mkfifo(full); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
}

// touchToClock sets the modification time of the file full to the time of the test clock.
func touchToClock(t *testing.T, full string) {
	t.Helper()
	if err := os.Chtimes(full, clockTime(), clockTime()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFileRacesAreRechecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, full string)
		want   error
	}{
		{"deleted", mustRemove, ErrNotFound},
		{"replaced by directory", replaceWithDirectory, ErrIsDir},
		{"replaced by pipe", replaceWithPipe, ErrNotFound},
		{"replaced by fresh file", touchToClock, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fix := newFixture(t)
			fix.write(t, "f", "x", epoch())
			share := fix.open(t, &Options{MinAge: time.Hour})
			share.beforeOpen = func(string) { tt.mutate(t, filepath.Join(fix.root, "f")) }
			file, err := share.OpenFile("f")
			if err == nil {
				mustClose(t, file)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("OpenFile = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestListOpenRace(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "d/x", "x", epoch())
	share := fix.open(t, &Options{})
	share.beforeOpen = func(string) {
		if err := os.RemoveAll(filepath.Join(fix.root, "d")); err != nil {
			t.Error(err)
		}
	}
	if _, err := share.List("d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List = %v, want ErrNotFound", err)
	}
}

// countedShare opens a share over a file "f" that holds "first", and counts the opens.
func countedShare(t *testing.T) (fixture, *Share, *openCounter) {
	t.Helper()
	fix := newFixture(t)
	fix.write(t, "f", "first", epoch())
	share := fix.open(t, &Options{MTimePrecision: time.Second})
	counter := &openCounter{}
	counter.attach(share)
	return fix, share, counter
}

func TestDigestsAreCached(t *testing.T) {
	t.Parallel()
	_, share, counter := countedShare(t)
	digests1, info1 := mustDigests(t, share, "f")
	digests2, info2 := mustDigests(t, share, "f")
	if counter.opens != 1 || digests1 != digests2 || info1 != info2 {
		t.Errorf("opens = %d, digests1 = %+v, digests2 = %+v", counter.opens, digests1, digests2)
	}
}

func TestDigestsAreReadAgainForANewModificationTime(t *testing.T) {
	t.Parallel()
	fix, share, counter := countedShare(t)
	digests1, info1 := mustDigests(t, share, "f")

	// Same size, new modification time: a different version.
	fix.write(t, "f", "other", epoch().Add(time.Minute))
	digests2, info2 := mustDigests(t, share, "f")
	if counter.opens != 2 || digests2.Head == digests1.Head || digests2.Tail == digests1.Tail ||
		info2.ETag == info1.ETag {
		t.Errorf("opens = %d, digests2 = %+v", counter.opens, digests2)
	}
}

func TestDigestsAreReadAgainForANewSize(t *testing.T) {
	t.Parallel()
	fix, share, counter := countedShare(t)
	fix.write(t, "f", "other", epoch().Add(time.Minute))
	digests1, _ := mustDigests(t, share, "f")

	// New size, same modification time.
	fix.write(t, "f", "longer content", epoch().Add(time.Minute))
	digests2, _ := mustDigests(t, share, "f")
	if counter.opens != 2 || digests2.Head == digests1.Head || digests2.Tail == digests1.Tail {
		t.Errorf("opens = %d, digests2 = %+v", counter.opens, digests2)
	}
}

// TestDigestsAreReadAgainForAFileReplacedWithinTheSameSecond covers a file that another file
// of the same size is renamed over, with the same modification time: path, size and the
// modification time cut to the second all stay the same.
func TestDigestsAreReadAgainForAFileReplacedWithinTheSameSecond(t *testing.T) {
	t.Parallel()
	fix, share, counter := countedShare(t)
	digests1, info1 := mustDigests(t, share, "f")

	fix.write(t, "replacement", "other", epoch())
	if err := os.Rename(filepath.Join(fix.root, "replacement"), filepath.Join(fix.root, "f")); err != nil {
		t.Fatal(err)
	}
	digests2, info2 := mustDigests(t, share, "f")
	if info2.Size != info1.Size || !info2.MTime.Equal(info1.MTime) {
		t.Fatalf("the test needs the same size and time: %+v, then %+v", info1, info2)
	}
	if counter.opens != 2 || digests2.Head == digests1.Head || digests2.Tail == digests1.Tail {
		t.Errorf("opens = %d, digests1 = %+v, digests2 = %+v", counter.opens, digests1, digests2)
	}

	// The new file is cached like any other.
	digests3, _ := mustDigests(t, share, "f")
	if counter.opens != 2 || digests3 != digests2 {
		t.Errorf("opens = %d, digests3 = %+v, want %+v", counter.opens, digests3, digests2)
	}
}

func TestHeadDigestCoversOnlyTheHead(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	head := strings.Repeat("h", 64<<10)
	fix.write(t, "a", head+"tail one", epoch())
	fix.write(t, "b", head+"tail two", epoch())
	share := fix.open(t, &Options{})
	digestsA, _ := mustDigests(t, share, "a")
	digestsB, _ := mustDigests(t, share, "b")
	if digestsA.Head != digestsB.Head {
		t.Error("head digests of equal heads differ")
	}
	if digestsA.Tail == digestsB.Tail {
		t.Error("tail digests of different tails are equal")
	}
}

func TestTailDigestCoversOnlyTheTail(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	tail := strings.Repeat("t", 64<<10)
	fix.write(t, "a", "head one"+tail, epoch())
	fix.write(t, "b", "head two"+tail, epoch())
	share := fix.open(t, &Options{})
	digestsA, _ := mustDigests(t, share, "a")
	digestsB, _ := mustDigests(t, share, "b")
	if digestsA.Tail != digestsB.Tail {
		t.Error("tail digests of equal tails differ")
	}
	if digestsA.Head == digestsB.Head {
		t.Error("head digests of different heads are equal")
	}
	sum := sha256.Sum256([]byte(tail))
	if want := hex.EncodeToString(sum[:]); digestsA.Tail != want {
		t.Errorf("tail digest = %s, want %s", digestsA.Tail, want)
	}
}

func TestDigestsOfShortAndEmptyFiles(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "short", "abc", epoch())
	fix.write(t, "exact", strings.Repeat("e", 64<<10), epoch())
	fix.write(t, "empty", "", epoch())
	share := fix.open(t, &Options{})
	// SHA-256 of "abc" and of nothing.
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const nothing = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for name, want := range map[string]string{"short": abc, "empty": nothing} {
		digests, _ := mustDigests(t, share, name)
		if digests.Head != want || digests.Tail != want {
			t.Errorf("%s: digests = %+v, want %s for both", name, digests, want)
		}
	}
	exact, _ := mustDigests(t, share, "exact")
	if exact.Head != exact.Tail {
		t.Errorf("a file of exactly 64 KiB: head %s, tail %s", exact.Head, exact.Tail)
	}
}

func TestDigestsCacheEvicts(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	for _, name := range []string{"a", "b", "c"} {
		fix.write(t, name, name, epoch())
	}
	share := fix.open(t, &Options{DigestCacheSize: 2})
	counter := &openCounter{}
	counter.attach(share)
	mustDigests(t, share, "a")
	mustDigests(t, share, "b")
	mustDigests(t, share, "c") // evicts a
	if counter.opens != 3 {
		t.Fatalf("opens = %d, want 3", counter.opens)
	}
	mustDigests(t, share, "c")
	mustDigests(t, share, "b")
	if counter.opens != 3 {
		t.Errorf("cached entries were read again: opens = %d", counter.opens)
	}
	mustDigests(t, share, "a")
	if counter.opens != 4 {
		t.Errorf("evicted entry was not read again: opens = %d", counter.opens)
	}
}

func TestDigestsOpenFailure(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "f", "x", epoch())
	share := fix.open(t, &Options{})
	share.beforeOpen = func(string) { mustRemove(t, filepath.Join(fix.root, "f")) }
	if _, _, err := share.Digests("f"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Digests = %v, want ErrNotFound", err)
	}
}

// failingReaderAt fails every read at the given offset or later, so a test can break the
// head read or the tail read on its own.
type failingReaderAt struct {
	from    int64
	content string
}

func (r failingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= r.from {
		return 0, errors.New("disk on fire")
	}
	return strings.NewReader(r.content).ReadAt(p, offset)
}

func TestDigestsReadError(t *testing.T) {
	t.Parallel()
	size := int64(2 * (64 << 10))
	content := strings.Repeat("x", int(size))
	if _, err := computeDigests(failingReaderAt{from: 0, content: content}, size); err == nil ||
		!strings.Contains(err.Error(), "head") {
		t.Errorf("computeDigests with a failing head = %v", err)
	}
	// The head ends at 64 KiB and the tail starts there, so a failure from 64 KiB on
	// leaves the head readable and breaks only the tail.
	if _, err := computeDigests(failingReaderAt{from: 64 << 10, content: content}, size); err == nil ||
		!strings.Contains(err.Error(), "tail") {
		t.Errorf("computeDigests with a failing tail = %v", err)
	}
	fix := newFixture(t)
	fix.write(t, "f", "x", epoch())
	share := fix.open(t, &Options{})
	share.computeDigests = func(io.ReaderAt, int64) (FileDigests, error) {
		return computeDigests(failingReaderAt{content: "x"}, 1)
	}
	_, _, err := share.Digests("f")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `share "test"`) {
		t.Errorf("Digests = %v", err)
	}
}

func TestFile(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "f", "0123456789", epoch())
	share := fix.open(t, &Options{})
	file, err := share.OpenFile("f")
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
	mustClose(t, file)
	if _, err := file.Read(buf); err == nil {
		t.Error("Read after Close succeeded")
	}
}

func TestFileErrorsNameTheFile(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "f", "0123456789", epoch())
	file, err := fix.open(t, &Options{}).OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, file)

	_, seekErr := file.Seek(0, io.SeekStart)
	if !errors.Is(seekErr, os.ErrClosed) || !strings.Contains(seekErr.Error(), `"f"`) {
		t.Errorf("Seek after Close = %v", seekErr)
	}
	closeErr := file.Close()
	if !errors.Is(closeErr, os.ErrClosed) || !strings.Contains(closeErr.Error(), `"f"`) {
		t.Errorf("second Close = %v", closeErr)
	}
}

type fakeDir struct {
	names []string
	err   error
}

func (dir fakeDir) Readdirnames(int) ([]string, error) { return dir.names, dir.err }

func TestCollect(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "d/real", "x", epoch())
	share := fix.open(t, &Options{})

	if _, err := share.collect("d", fakeDir{err: errors.New("read failed")}); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("collect with a read error = %v", err)
	}
	// An entry that vanished after the directory was read is skipped.
	got, err := share.collect("d", fakeDir{names: []string{"vanished", "real"}})
	if err != nil || !slices.Equal(names(got), []string{"real"}) {
		t.Errorf("collect = %v, %v", names(got), err)
	}
	// An entry that cannot be examined fails the listing.
	long := strings.Repeat("a", 300)
	_, err = share.collect("d", fakeDir{names: []string{long}})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("collect with an unreadable entry = %v", err)
	}
	if !strings.Contains(err.Error(), `share "test"`) || !strings.Contains(err.Error(), long) {
		t.Errorf("error %q lacks share name or path", err)
	}
}

type fakeHandle struct{ err error }

func (handle fakeHandle) Stat() (os.FileInfo, error) { return nil, handle.err }

func TestOpenInfoStatError(t *testing.T) {
	t.Parallel()
	share := newFixture(t).open(t, &Options{})
	_, _, err := share.openInfo("f", fakeHandle{err: errors.New("stat failed")})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("openInfo = %v", err)
	}
}

func TestAbsent(t *testing.T) {
	t.Parallel()
	pathError := func(err error) error { return &fs.PathError{Op: "x", Path: "p", Err: err} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"not exist", pathError(fs.ErrNotExist), true},
		{"not a directory", pathError(errSyscall("ENOTDIR")), true},
		{"loop", pathError(errSyscall("ELOOP")), true},
		{"escape", pathError(errors.New("path escapes from parent")), true},
		{"permission", pathError(fs.ErrPermission), false},
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
	space, err := newFixture(t).open(t, &Options{}).Space()
	if err != nil {
		t.Fatal(err)
	}
	if space.Total == 0 || space.Free > space.Total {
		t.Errorf("Space = %+v", space)
	}
}

func TestSpaceError(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	share := fix.open(t, &Options{})
	share.space = func(string) (wireSpace, error) { return wireSpace{}, errors.New("statfs failed") }
	if _, err := share.Space(); err == nil || !strings.Contains(err.Error(), `share "test"`) {
		t.Errorf("Space = %v", err)
	}
	if _, err := statSpace(filepath.Join(fix.base, "missing")); err == nil {
		t.Error("statSpace on a missing path succeeded")
	}
}

// useShareLikeAClient calls every operation of the share on each of the files, again and again.
func useShareLikeAClient(t *testing.T, share *Share, files []string) {
	t.Helper()
	for range 20 {
		for _, name := range files {
			if _, _, err := share.Digests(name); err != nil {
				t.Error(err)
			}
			if _, err := share.Stat(name); err != nil {
				t.Error(err)
			}
		}
		if _, err := share.List(""); err != nil {
			t.Error(err)
		}
		if _, err := share.Space(); err != nil {
			t.Error(err)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	files := []string{"a", "b", "c", "d/e"}
	for _, name := range files {
		fix.write(t, name, name, epoch())
	}
	share := fix.open(t, &Options{MTimePrecision: time.Second, DigestCacheSize: 2})
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { useShareLikeAClient(t, share, files) })
	}
	group.Wait()
}

func TestBackslashIsAnOrdinaryCharacter(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, `odd\name`, "content", epoch())
	share := fix.open(t, &Options{})
	requireListing(t, share, "", []string{`odd\name`})
	if got := readFile(t, share, `odd\name`); got != "content" {
		t.Errorf("read = %q", got)
	}
}

func TestListHidesNamesThatAreNotUTF8(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.write(t, "d/real", "x", epoch())
	share := fix.open(t, &Options{})
	got, err := share.collect("d", fakeDir{names: []string{"bad\xff", "real"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("collect = %+v, want only the valid name", got)
	}
}
