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

	"github.com/BlueRival/splicefs/internal/wire"
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
	s := &Share{
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
	if s.now == nil {
		s.now = time.Now
	}
	size := opts.DigestCacheSize
	if size <= 0 {
		size = defaultDigestCacheSize
	}
	s.digests = newLRU(size)
	return s, nil
}

// Close releases the share root.
func (s *Share) Close() error {
	return s.root.Close()
}

// Name returns the share name.
func (s *Share) Name() string {
	return s.name
}

// Stat describes the file or directory at name.
func (s *Share) Stat(name string) (Info, error) {
	p, fi, err := s.lookup(name)
	if err != nil {
		return Info{}, err
	}
	return s.info(path.Base(p), fi), nil
}

// List returns the visible entries of the directory at name, sorted by name in
// byte order. It never opens the files it lists.
func (s *Share) List(name string) ([]Info, error) {
	p, fi, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrNotDir
	}
	s.beforeOpen(p)
	d, err := s.root.OpenFile(p, openFlags, 0)
	if err != nil {
		return nil, s.fail(p, err)
	}
	defer func() { _ = d.Close() }()
	return s.collect(p, d)
}

// dirReader is the part of a directory handle that List needs.
type dirReader interface {
	Readdirnames(n int) ([]string, error)
}

// collect builds the sorted listing of the directory p from its entry names.
func (s *Share) collect(p string, d dirReader) ([]Info, error) {
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, s.fail(p, err)
	}
	slices.Sort(names)
	infos := make([]Info, 0, len(names))
	for _, n := range names {
		// A name that is not valid UTF-8 cannot be written in a JSON listing or
		// asked for by a client, so it is hidden like any other unservable entry.
		if s.excluded(n) || !utf8.ValidString(n) {
			continue
		}
		fi, err := s.statEntry(path.Join(p, n))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.visible(fi) {
			infos = append(infos, s.info(n, fi))
		}
	}
	return infos, nil
}

// OpenFile opens the regular file at name for reading. The returned Info describes
// the file that was actually opened.
func (s *Share) OpenFile(name string) (*File, error) {
	p, fi, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, ErrIsDir
	}
	s.beforeOpen(p)
	f, err := s.root.OpenFile(p, openFlags, 0)
	if err != nil {
		return nil, s.fail(p, err)
	}
	info, err := s.openInfo(p, f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &File{f: f, info: info}, nil
}

// handle is the part of an open file that openInfo needs.
type handle interface {
	Stat() (os.FileInfo, error)
}

// openInfo checks the open file h itself and describes it.
func (s *Share) openInfo(p string, h handle) (Info, error) {
	fi, err := h.Stat()
	if err != nil {
		return Info{}, s.fail(p, err)
	}
	if fi.IsDir() {
		return Info{}, ErrIsDir
	}
	if !s.visible(fi) {
		return Info{}, ErrNotFound
	}
	return s.info(path.Base(p), fi), nil
}

// HeadDigest returns the hex SHA-256 of the first wire.HeadDigestBytes bytes of the
// file at name, and the file's Info. A file is read once until its size or
// modification time changes.
func (s *Share) HeadDigest(name string) (string, Info, error) {
	p, fi, err := s.lookup(name)
	if err != nil {
		return "", Info{}, err
	}
	if fi.IsDir() {
		return "", Info{}, ErrIsDir
	}
	info := s.info(path.Base(p), fi)
	if digest, ok := s.digests.get(newDigestKey(p, info)); ok {
		return digest, info, nil
	}
	f, err := s.OpenFile(name)
	if err != nil {
		return "", Info{}, err
	}
	defer func() { _ = f.Close() }()
	digest, err := s.headDigest(f)
	if err != nil {
		return "", Info{}, fmt.Errorf("share %q: %s: read head: %w", s.name, p, err)
	}
	info = f.Info()
	s.digests.put(newDigestKey(p, info), digest)
	return digest, info, nil
}

// Space reports the size and free space of the file system holding the share.
func (s *Share) Space() (wire.Space, error) {
	sp, err := s.space(s.path)
	if err != nil {
		return wire.Space{}, fmt.Errorf("share %q: space: %w", s.name, err)
	}
	return sp, nil
}

// excluded reports whether name matches an exclude pattern.
func (s *Share) excluded(name string) bool {
	for _, pattern := range s.exclude {
		// Open has validated every pattern, so Match cannot fail.
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// visible reports whether an entry of this type and age is shown.
func (s *Share) visible(fi os.FileInfo) bool {
	switch {
	case fi.IsDir():
		return true
	case !fi.Mode().IsRegular():
		return false
	case s.minAge > 0:
		return !fi.ModTime().After(s.now().Add(-s.minAge))
	}
	return true
}

// info converts file information to an Info.
func (s *Share) info(name string, fi os.FileInfo) Info {
	i := Info{Name: name, Dir: fi.IsDir(), MTime: s.truncate(fi.ModTime())}
	if !i.Dir {
		i.Size = fi.Size()
		i.ETag = ETag(i.Size, i.MTime)
	}
	return i
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
