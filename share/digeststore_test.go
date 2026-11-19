package share

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/server/internal/digestcache"
	"github.com/SpliceFS/server/internal/filestat"
)

// storedShare is a share over a file "f" that holds "first", with a digest store in a folder
// of its own, a log that the test reads and a counter of the reads of digests.
type storedShare struct {
	fix     fixture
	share   *Share
	store   *digestcache.Store
	digests string
	log     *bytes.Buffer
	counter *readCounter
}

func newStoredShare(t *testing.T) storedShare {
	t.Helper()
	fix := newFixture(t)
	fix.write(t, "f", "first", epoch())
	digests := filepath.Join(fix.base, "digests")
	if err := os.Mkdir(digests, 0o700); err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	store, err := digestcache.Open(digests, slog.New(slog.NewTextHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustClose(t, store) })
	share := fix.open(t, &Options{DigestStore: store})
	counter := &readCounter{}
	counter.attach(share)
	return storedShare{fix: fix, share: share, store: store, digests: digests, log: log, counter: counter}
}

// stamp returns the stamp the file at rel in the share has now.
func (s storedShare) stamp(t *testing.T, rel string) filestat.Stamp {
	t.Helper()
	stamp, err := filestat.Stat(filepath.Join(s.fix.root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

// entryFile is where the entry of the file at rel in the share is.
func (s storedShare) entryFile(rel string) string {
	return filepath.Join(s.digests, "test", rel+".splicefs_state")
}

// stored reports whether the store answers for the file at rel as it is now with want.
func (s storedShare) stored(t *testing.T, rel string, want FileDigests) bool {
	t.Helper()
	got, ok := s.store.Load("test", rel, s.stamp(t, rel))
	return ok && got == want
}

// warnings counts the warnings logged so far.
func (s storedShare) warnings() int {
	return strings.Count(s.log.String(), "level=WARN")
}

// planted are digests no read of a file gives, so a test can tell an answer from an entry.
func planted() FileDigests {
	return FileDigests{Head: strings.Repeat("a", 64), Tail: strings.Repeat("b", 64)}
}

func TestDigestsUsesEntry(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	if err := fixed.store.Save("test", "f", fixed.stamp(t, "f"), planted()); err != nil {
		t.Fatal(err)
	}
	if got, _ := mustDigests(t, fixed.share, "f"); got != planted() || fixed.counter.reads != 0 {
		t.Errorf("Digests = %+v after %d reads, want the entry and no read", got, fixed.counter.reads)
	}
}

func TestDigestsStampChangeRecomputes(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	if err := fixed.store.Save("test", "f", fixed.stamp(t, "f"), planted()); err != nil {
		t.Fatal(err)
	}
	fixed.fix.write(t, "f", "other", epoch().Add(time.Minute))
	got, _ := mustDigests(t, fixed.share, "f")
	if got == planted() || fixed.counter.reads != 1 || !fixed.stored(t, "f", got) {
		t.Errorf("Digests = %+v after %d reads, want a read and a new entry", got, fixed.counter.reads)
	}
}

func TestDigestsCorruptRecomputes(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	if err := os.MkdirAll(filepath.Dir(fixed.entryFile("f")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixed.entryFile("f"), []byte("version: 1\n{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := mustDigests(t, fixed.share, "f")
	if fixed.counter.reads != 1 || !fixed.stored(t, "f", got) {
		t.Errorf("Digests = %+v after %d reads, want a read that replaces the entry", got, fixed.counter.reads)
	}
}

// blockSaves makes every save of the share fail: a file stands where its folder goes.
func (s storedShare) blockSaves(t *testing.T) {
	t.Helper()
	folder := filepath.Join(s.digests, "test")
	if err := errors.Join(os.RemoveAll(folder), os.WriteFile(folder, nil, 0o600)); err != nil {
		t.Fatal(err)
	}
}

func TestDigestsUnwritableStillAnswers(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	fixed.blockSaves(t)
	want, err := computeDigests(strings.NewReader("first"), int64(len("first")))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mustDigests(t, fixed.share, "f"); got != want {
		t.Errorf("Digests = %+v, want %+v", got, want)
	}
}

// A file that changes while its digests are read gets no entry: the stamp taken before the
// read would be paired with bytes of another version. The change happens inside the read, so
// it falls between the two stamps every time.
func TestDigestsChangedDuringReadNotStored(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	before := fixed.stamp(t, "f")
	read := fixed.share.computeDigests
	fixed.share.computeDigests = func(r io.ReaderAt, size int64) (FileDigests, error) {
		digests, err := read(r, size)
		fixed.fix.write(t, "f", "other", epoch().Add(time.Minute))
		return digests, err
	}
	first, _ := mustDigests(t, fixed.share, "f")
	if _, ok := fixed.store.Load("test", "f", before); ok || fixed.stored(t, "f", first) {
		t.Error("an entry was saved for a file that changed while it was read")
	}
	if _, ok := fixed.share.digests.get(digestcache.KeyOf("f", before)); ok {
		t.Error("the memory cache kept digests of a file that changed while it was read")
	}
}

// A memory hit answers without the store: an entry planted after the first request for the
// same stamp is never seen.
func TestDigestsMemoryHitSkipsStore(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	first, _ := mustDigests(t, fixed.share, "f")
	if err := fixed.store.Save("test", "f", fixed.stamp(t, "f"), planted()); err != nil {
		t.Fatal(err)
	}
	if second, _ := mustDigests(t, fixed.share, "f"); second != first || fixed.counter.reads != 1 {
		t.Errorf("Digests = %+v after %d reads, want %+v from memory", second, fixed.counter.reads, first)
	}
}

// A failed save is logged once, and again only after a save has worked in between.
// The warning is the node's, not a share's: a full state folder fails the saves of every
// share, and one warning says so.
func TestDigestsWarnOncePerStore(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	other, err := Open(Options{Name: "other", Path: fixed.fix.root, DigestStore: fixed.store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustClose(t, other) })
	fixed.blockSaves(t)
	if err = os.WriteFile(filepath.Join(fixed.digests, "other"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mustDigests(t, fixed.share, "f")
	mustDigests(t, other, "f")
	if fixed.warnings() != 1 {
		t.Errorf("%d warnings after failed saves of two shares, want 1:\n%s", fixed.warnings(), fixed.log)
	}
}

func TestDigestsStampFailure(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	fixed.share.statFile = func(*os.File) (filestat.Stamp, error) { return filestat.Stamp{}, errors.New("disk on fire") }
	if _, _, err := fixed.share.Digests("f"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Digests = %v, want the failure", err)
	}
}

// The Info of an open file comes from the stamp that the digests are keyed on, so a request
// reads the file system once and the tag and the digests always describe the same moment.
func TestOpenFileInfoFromStamp(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	stamp := fixed.stamp(t, "f")
	stamp.Size = 12345
	stamp.Mtime = epoch().Add(time.Hour).UnixNano()
	calls := 0
	fixed.share.statFile = func(*os.File) (filestat.Stamp, error) {
		calls++
		return stamp, nil
	}
	_, info := mustDigests(t, fixed.share, "f")
	if info.Size != stamp.Size || !info.MTime.Equal(epoch().Add(time.Hour)) {
		t.Errorf("Info = %+v, want size and time of the stamp", info)
	}
	mustDigests(t, fixed.share, "f")
	if calls != 3 {
		t.Errorf("%d stamps for a read and a memory hit, want 3", calls)
	}
}

// Stamp finds what a request finds: the stamp of a visible file, nothing for a file that is
// gone, hidden or a folder, and an error when it cannot tell.
func TestStamp(t *testing.T) {
	t.Parallel()
	fixed := newStoredShare(t)
	fixed.fix.write(t, ".hidden", "x", epoch())
	fixed.fix.write(t, "d/f", "x", epoch())
	hiding := fixed.fix.open(t, &Options{Exclude: []string{".*"}})
	if stamp, found, err := hiding.Stamp("f"); err != nil || !found || stamp != fixed.stamp(t, "f") {
		t.Errorf("Stamp(f) = %+v, %v, %v, want the stamp of the file", stamp, found, err)
	}
	for _, name := range []string{"gone", ".hidden", "d"} {
		if _, found, err := hiding.Stamp(name); err != nil || found {
			t.Errorf("Stamp(%q) = %v, %v, want not found", name, found, err)
		}
	}
	hiding.statFile = func(*os.File) (filestat.Stamp, error) { return filestat.Stamp{}, errors.New("disk on fire") }
	if _, found, err := hiding.Stamp("f"); err == nil || found {
		t.Errorf("Stamp with a failing stat = %v, %v, want the failure", found, err)
	}
}
