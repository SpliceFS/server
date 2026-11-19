// Package share gives the SpliceFS server safe, read-only access to one shared
// directory. Every path is resolved inside the share root with os.Root, so nothing
// outside the root is ever listed, statted or opened, whatever the request path and
// whatever symbolic links exist. Names that are hidden by the share's rules behave
// exactly as if they did not exist.
package share

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SpliceFS/common/wire"

	"github.com/SpliceFS/server/internal/digestcache"
	"github.com/SpliceFS/server/internal/filestat"
)

// defaultDigestCacheSize is the digest cache size used when Options leaves it 0.
const defaultDigestCacheSize = 65536

var (
	// ErrNotFound means the name is missing, hidden, outside the share or invalid.
	ErrNotFound = errors.New("not found")
	// ErrNotDir means List was called on something that is not a directory.
	ErrNotDir = errors.New("not a directory")
	// ErrIsDir means OpenFile or Digests was called on a directory.
	ErrIsDir = errors.New("is a directory")
)

// Options configures a Share.
type Options struct {
	// Name is the share name, used in error messages.
	Name string
	// Path is the absolute directory to share.
	Path string
	// Exclude holds path.Match patterns applied to every name on the path.
	Exclude []string
	// MinAge hides regular files modified more recently than this. 0 disables it.
	MinAge time.Duration
	// FollowSymlinks false hides symbolic links. True follows links that resolve
	// inside the share and hides the others.
	FollowSymlinks bool
	// MTimePrecision is what modification times are truncated to, for example
	// time.Second. 0 keeps the full resolution of the file system.
	MTimePrecision time.Duration
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
	// DigestCacheSize is the number of entries kept by the digest cache.
	// 0 means a default of 65536.
	DigestCacheSize int
	// DigestStore keeps the digests in files of the state folder, so a restart does not
	// read the files again. nil keeps them in memory only.
	DigestStore *digestcache.Store
}

// Info describes a file or directory in a share.
type Info struct {
	// Name is the base name. It is "." for the share root.
	Name string
	// Dir is true for directories.
	Dir bool
	// Size is the size in bytes, 0 for directories.
	Size int64
	// MTime is the modification time, truncated to the share's precision, in UTC.
	MTime time.Time
	// ETag is the quoted strong tag of a file, "" for directories.
	ETag string
}

// Share is read-only access to one shared directory. It is safe for concurrent use.
type Share struct {
	name      string
	path      string
	root      *os.Root
	exclude   []string
	minAge    time.Duration
	follow    bool
	precision time.Duration
	now       func() time.Time
	digests   *lru
	store     *digestcache.Store

	// space reports the file system usage for a path. Tests replace it.
	space func(string) (wire.Space, error)
	// computeDigests computes the head and tail digests of a file of the given size.
	// Tests replace it.
	computeDigests func(io.ReaderAt, int64) (FileDigests, error)
	// statFile reads the stamp of an open file. Tests replace it.
	statFile func(*os.File) (filestat.Stamp, error)
	// beforeOpen runs after a name has been resolved and before it is opened.
	// Tests use it to change the file system at exactly that moment.
	beforeOpen func(string)
}

// Open validates opts and opens the share. It fails if Path is not an existing
// directory or an Exclude pattern is invalid.
func Open(opts Options) (*Share, error) {
	for _, pattern := range opts.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("share %q: exclude pattern %q: %w", opts.Name, pattern, err)
		}
	}
	root, err := os.OpenRoot(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("share %q: open root: %w", opts.Name, err)
	}
	share := &Share{
		name:           opts.Name,
		path:           opts.Path,
		root:           root,
		exclude:        slices.Clone(opts.Exclude),
		minAge:         opts.MinAge,
		follow:         opts.FollowSymlinks,
		precision:      opts.MTimePrecision,
		now:            opts.Now,
		store:          opts.DigestStore,
		space:          statSpace,
		computeDigests: computeDigests,
		statFile:       filestat.StatFile,
		beforeOpen:     func(string) {},
	}
	if share.now == nil {
		share.now = time.Now
	}
	size := opts.DigestCacheSize
	if size <= 0 {
		size = defaultDigestCacheSize
	}
	share.digests = newLRU(size)
	return share, nil
}

// Close releases the share root.
func (s *Share) Close() error {
	return s.root.Close() //nolint:wrapcheck // the caller names the share, and the error says the rest
}

// Name returns the share name.
func (s *Share) Name() string {
	return s.name
}

// Stat describes the file or directory at name.
func (s *Share) Stat(name string) (Info, error) {
	target, err := s.lookup(name)
	if err != nil {
		return Info{}, err
	}
	return s.info(path.Base(target.relPath), factsOfInfo(target.info)), nil
}

// List returns the visible entries of the directory at name, sorted by name in
// byte order. It never opens the files it lists.
func (s *Share) List(name string) ([]Info, error) {
	target, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if !target.info.IsDir() {
		return nil, ErrNotDir
	}
	s.beforeOpen(target.relPath)
	dir, err := s.root.OpenFile(target.relPath, openFlags, 0)
	if err != nil {
		return nil, s.fail(target.relPath, err)
	}
	defer closeRead(dir)
	return s.collect(target.relPath, dir)
}

// OpenFile opens the regular file at name for reading. The returned Info describes
// the file that was actually opened.
func (s *Share) OpenFile(name string) (*File, error) {
	target, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if target.info.IsDir() {
		return nil, ErrIsDir
	}
	s.beforeOpen(target.relPath)
	file, err := s.root.OpenFile(target.relPath, openFlags, 0)
	if err != nil {
		return nil, s.fail(target.relPath, err)
	}
	stamp, err := s.statFile(file)
	if err != nil {
		closeRead(file)
		return nil, s.fail(target.relPath, err)
	}
	info, err := s.openInfo(target.relPath, stamp)
	if err != nil {
		closeRead(file)
		return nil, err
	}
	return &File{f: file, info: info, stamp: stamp, relPath: target.relPath}, nil
}

// Digests returns the hex SHA-256 of the first wire.HeadDigestBytes bytes (the head
// digest) and of the last wire.TailDigestBytes bytes (the tail digest) of the file at
// name, and the file's Info. For a file of at most 64 KiB both cover the whole file. A
// file is read once until it is changed or replaced: the in-memory cache and the digest
// store are keyed on the stamp of the open file (inode, size, and modification, change and
// creation time, all at full precision), so a replacement of the same size within the same
// second is seen too. See "Digest cache" in docs/design.md.
func (s *Share) Digests(name string) (FileDigests, Info, error) {
	file, err := s.OpenFile(name)
	if err != nil {
		return FileDigests{}, Info{}, err
	}
	defer closeRead(file)
	digests, err := s.digestsOf(file)
	if err != nil {
		return FileDigests{}, Info{}, err
	}
	return digests, file.Info(), nil
}

// Space reports the size and free space of the file system holding the share.
func (s *Share) Space() (wire.Space, error) {
	space, err := s.space(s.path)
	if err != nil {
		return wire.Space{}, fmt.Errorf("share %q: space: %w", s.name, err)
	}
	return space, nil
}

// Stamp returns the stamp of the regular file at name as a request finds it, and false when
// a request would not find it: it is gone, hidden or a folder. The digest store's sweep uses
// it to tell which entries are still of use.
func (s *Share) Stamp(name string) (filestat.Stamp, bool, error) {
	file, err := s.OpenFile(name)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrIsDir) {
		return filestat.Stamp{}, false, nil
	}
	if err != nil {
		return filestat.Stamp{}, false, err
	}
	defer closeRead(file)
	return file.stamp, true, nil
}

// digestsOf returns the digests of the open file, which had its stamp when it was opened:
// from memory, else from the store, else read from the file.
func (s *Share) digestsOf(file *File) (FileDigests, error) {
	stamp := file.stamp
	key := digestcache.KeyOf(file.relPath, stamp)
	if cached, ok := s.digests.get(key); ok {
		return cached, nil
	}
	if s.store != nil {
		if stored, ok := s.store.Load(s.name, file.relPath, stamp); ok {
			s.digests.put(key, stored)
			return stored, nil
		}
	}
	digests, err := s.computeDigests(file.f, stamp.Size)
	if err != nil {
		return FileDigests{}, fmt.Errorf("share %q: %s: read digests: %w", s.name, file.relPath, err)
	}
	// The digests are kept only when the file has the same stamp after the read as before
	// it; otherwise they may be of bytes from two versions, and the answer is all they are
	// good for.
	if again, err := s.statFile(file.f); err == nil && again == stamp {
		s.digests.put(key, digests)
		if s.store != nil {
			s.store.Keep(s.name, file.relPath, stamp, digests)
		}
	}
	return digests, nil
}

// dirReader is the part of a directory handle that List needs.
type dirReader interface {
	Readdirnames(n int) ([]string, error)
}

// collect builds the sorted listing of the directory relPath from its entry names.
func (s *Share) collect(relPath string, dir dirReader) ([]Info, error) {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, s.fail(relPath, err)
	}
	slices.Sort(names)
	infos := make([]Info, 0, len(names))
	for _, name := range names {
		// A name that is not valid UTF-8 cannot be written in a JSON listing or
		// asked for by a client, so it is hidden like any other unservable entry.
		if s.excluded(name) || !utf8.ValidString(name) {
			continue
		}
		fileInfo, err := s.statEntry(path.Join(relPath, name))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if entry := factsOfInfo(fileInfo); s.visible(entry) {
			infos = append(infos, s.info(name, entry))
		}
	}
	return infos, nil
}

// openInfo checks the open file at relPath, which has stamp, and describes it.
func (s *Share) openInfo(relPath string, stamp filestat.Stamp) (Info, error) {
	opened := factsOfStamp(stamp)
	if opened.mode.IsDir() {
		return Info{}, ErrIsDir
	}
	if !s.visible(opened) {
		return Info{}, ErrNotFound
	}
	return s.info(path.Base(relPath), opened), nil
}

// facts is what decides whether an entry is shown and how it is described, whether it was
// read by name or from an open file.
type facts struct {
	mode  fs.FileMode
	size  int64
	mtime time.Time
}

// factsOfInfo takes the facts from a FileInfo.
func factsOfInfo(fileInfo os.FileInfo) facts {
	return facts{mode: fileInfo.Mode(), size: fileInfo.Size(), mtime: fileInfo.ModTime()}
}

// factsOfStamp takes the facts from a Stamp.
func factsOfStamp(stamp filestat.Stamp) facts {
	return facts{mode: stamp.Mode, size: stamp.Size, mtime: time.Unix(0, stamp.Mtime)}
}

// excluded reports whether name matches an exclude pattern.
func (s *Share) excluded(name string) bool {
	for _, pattern := range s.exclude {
		// Open has rejected every invalid pattern, so an error here means no match.
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	return false
}

// visible reports whether an entry of this type and age is shown.
func (s *Share) visible(entry facts) bool {
	switch {
	case entry.mode.IsDir():
		return true
	case !entry.mode.IsRegular():
		return false
	case s.minAge > 0:
		return !entry.mtime.After(s.now().Add(-s.minAge))
	}
	return true
}

// info converts the facts of an entry to an Info.
func (s *Share) info(name string, entry facts) Info {
	info := Info{Name: name, Dir: entry.mode.IsDir(), MTime: s.truncate(entry.mtime)}
	if !info.Dir {
		info.Size = entry.size
		info.ETag = ETag(info.Size, info.MTime)
	}
	return info
}

// truncate applies the modification time precision and converts to UTC.
func (s *Share) truncate(t time.Time) time.Time {
	if s.precision > 0 {
		t = t.Truncate(s.precision)
	}
	return t.UTC()
}

// cleanName turns a request name into a path relative to the root, or reports that
// it is not acceptable. The root is ".".
func cleanName(name string) (string, bool) {
	if name == "" || name == "." {
		return ".", true
	}
	if strings.ContainsRune(name, 0) || !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

// closeRead closes a handle that was only read. A failed close cannot lose data, so
// the error carries nothing a caller could act on.
func closeRead(closer io.Closer) {
	_ = closer.Close() //nolint:errcheck // the handle was only read, so a failed close loses nothing
}
