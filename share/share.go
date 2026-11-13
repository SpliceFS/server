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
)

// defaultDigestCacheSize is the head digest cache size used when Options leaves it 0.
const defaultDigestCacheSize = 65536

var (
	// ErrNotFound means the name is missing, hidden, outside the share or invalid.
	ErrNotFound = errors.New("not found")
	// ErrNotDir means List was called on something that is not a directory.
	ErrNotDir = errors.New("not a directory")
	// ErrIsDir means OpenFile or HeadDigest was called on a directory.
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
	// DigestCacheSize is the number of entries kept by the head digest cache.
	// 0 means a default of 65536.
	DigestCacheSize int
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

	// space reports the file system usage for a path. Tests replace it.
	space func(string) (wire.Space, error)
	// headDigest computes the digest of a file's head. Tests replace it.
	headDigest func(io.Reader) (string, error)
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
		name:       opts.Name,
		path:       opts.Path,
		root:       root,
		exclude:    slices.Clone(opts.Exclude),
		minAge:     opts.MinAge,
		follow:     opts.FollowSymlinks,
		precision:  opts.MTimePrecision,
		now:        opts.Now,
		space:      statSpace,
		headDigest: headDigest,
		beforeOpen: func(string) {},
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
	return s.info(path.Base(target.relPath), target.info), nil
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
	info, err := s.openInfo(target.relPath, file)
	if err != nil {
		closeRead(file)
		return nil, err
	}
	return &File{f: file, info: info}, nil
}

// HeadDigest returns the hex SHA-256 of the first wire.HeadDigestBytes bytes of the
// file at name, and the file's Info. A file is read once until its size or
// modification time changes.
func (s *Share) HeadDigest(name string) (string, Info, error) {
	target, err := s.lookup(name)
	if err != nil {
		return "", Info{}, err
	}
	if target.info.IsDir() {
		return "", Info{}, ErrIsDir
	}
	info := s.info(path.Base(target.relPath), target.info)
	if digest, ok := s.digests.get(newDigestKey(target.relPath, info)); ok {
		return digest, info, nil
	}
	file, err := s.OpenFile(name)
	if err != nil {
		return "", Info{}, err
	}
	defer closeRead(file)
	digest, err := s.headDigest(file)
	if err != nil {
		return "", Info{}, fmt.Errorf("share %q: %s: read head: %w", s.name, target.relPath, err)
	}
	info = file.Info()
	s.digests.put(newDigestKey(target.relPath, info), digest)
	return digest, info, nil
}

// Space reports the size and free space of the file system holding the share.
func (s *Share) Space() (wire.Space, error) {
	space, err := s.space(s.path)
	if err != nil {
		return wire.Space{}, fmt.Errorf("share %q: space: %w", s.name, err)
	}
	return space, nil
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
		if s.visible(fileInfo) {
			infos = append(infos, s.info(name, fileInfo))
		}
	}
	return infos, nil
}

// handle is the part of an open file that openInfo needs.
type handle interface {
	Stat() (os.FileInfo, error)
}

// openInfo checks the open file h itself and describes it.
func (s *Share) openInfo(relPath string, h handle) (Info, error) {
	fileInfo, err := h.Stat()
	if err != nil {
		return Info{}, s.fail(relPath, err)
	}
	if fileInfo.IsDir() {
		return Info{}, ErrIsDir
	}
	if !s.visible(fileInfo) {
		return Info{}, ErrNotFound
	}
	return s.info(path.Base(relPath), fileInfo), nil
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
func (s *Share) visible(fileInfo os.FileInfo) bool {
	switch {
	case fileInfo.IsDir():
		return true
	case !fileInfo.Mode().IsRegular():
		return false
	case s.minAge > 0:
		return !fileInfo.ModTime().After(s.now().Add(-s.minAge))
	}
	return true
}

// info converts file information to an Info.
func (s *Share) info(name string, fileInfo os.FileInfo) Info {
	info := Info{Name: name, Dir: fileInfo.IsDir(), MTime: s.truncate(fileInfo.ModTime())}
	if !info.Dir {
		info.Size = fileInfo.Size()
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
