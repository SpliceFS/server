package datadir

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SpliceFS/server/internal/filestat"
)

// Entry is the content of one file of a directory.
type Entry[T any] struct {
	// Name is the file name, with its extension.
	Name  string
	Value T
	// Stamp is the stamp of the file as Dir last looked at it. It is the stamp of a file that
	// failed to parse, whose Value is the last good one, so a reader that keeps the Value
	// compares it with StampOf the file to know when to ask again.
	Stamp Stamp
}

// Stamp is what tells that a file changed. Size and modification time are not enough: the
// files of a directory (a password hash, a key, a node's announcement) have a fixed length,
// the file system clock ticks coarsely, and a sync tool or a program sets the modification
// time of a file it writes. A rewrite within one tick, or with the old time put back, would
// then never be read. So the stamp also holds the file's identity: a file renamed over
// another has another inode, and a file written in place has a later change time.
type Stamp struct {
	size    int64
	modTime time.Time
	id      filestat.Identity
}

// StampOf returns the stamp of the file that info describes.
func StampOf(info fs.FileInfo) Stamp {
	return stampWith(info, filestat.Of)
}

// stampWith is StampOf with the identity read by identify, which a test replaces to set an
// identity the clock cannot.
func stampWith(info fs.FileInfo, identify func(fs.FileInfo) (filestat.Identity, bool)) Stamp {
	// Stat on a real file always carries an identity. Where it did not, the zero identity
	// leaves size and time as the whole stamp.
	id, _ := identify(info)
	return Stamp{size: info.Size(), modTime: info.ModTime(), id: id}
}

// Dir follows a directory of YAML files. Each use lists the directory, compares names and
// stamps with what it has loaded and reads only the files that changed, so no signal is
// needed when another program changes the directory. It is not safe for concurrent use;
// the caller serializes calls.
//
// A file that cannot be read or parsed is ignored with a warning and the last good content
// of that file stays in use, so one bad file never affects another and never stops the
// server. Files whose name does not end in ".yaml", or starts with a dot (sync tools leave
// temporary files like that), are not files of the directory.
type Dir[T any] struct {
	dir   string
	parse func(name string, data []byte) (T, error)
	log   *slog.Logger
	read  func(path string) ([]byte, error)
	// identify is filestat.Of; a test replaces it to set an identity the clock cannot.
	identify func(fs.FileInfo) (filestat.Identity, bool)
	stamps   map[string]Stamp
	values   map[string]T
}

// NewDir returns a Dir for dir. parse turns the content of the named file into a value; its
// error text is logged, so it must never contain content that is secret.
func NewDir[T any](
	dir string, parse func(name string, data []byte) (T, error), log *slog.Logger,
) *Dir[T] {
	return &Dir[T]{
		dir: dir, parse: parse, log: log, read: os.ReadFile, identify: filestat.Of,
		stamps: map[string]Stamp{}, values: map[string]T{},
	}
}

// Refresh brings what is loaded up to date and returns the files' values sorted by file
// name, and whether any value was added, replaced or removed since the last call. A
// directory that does not exist has no files.
func (d *Dir[T]) Refresh() ([]Entry[T], bool) {
	var changed bool
	if found, ok := d.scan(); ok {
		changed = d.drop(found)
		for name, st := range found {
			if old, seen := d.stamps[name]; !seen || old != st {
				d.stamps[name] = st
				changed = d.load(name) || changed
			}
		}
	}
	return d.sorted(), changed
}

// scan returns the stamps of the files now in the directory. It is not ok when the
// directory cannot be listed, in which case what is loaded stays as it is.
func (d *Dir[T]) scan() (map[string]Stamp, bool) {
	items, err := os.ReadDir(d.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.log.Warn("cannot list directory, keeping what was loaded",
			slog.String("dir", d.dir), slog.Any("error", err))
		return nil, false
	}
	found := map[string]Stamp{}
	for _, item := range items {
		name := item.Name()
		if !strings.HasSuffix(name, FileExt) || strings.HasPrefix(name, ".") {
			continue
		}
		// Stat follows links, as a sync tool's link to a file should work; a directory
		// named like a file is not one.
		info, err := os.Stat(filepath.Join(d.dir, name))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		found[name] = stampWith(info, d.identify)
	}
	return found, true
}

// drop forgets the files that are gone and reports whether any had a value.
func (d *Dir[T]) drop(found map[string]Stamp) bool {
	var changed bool
	for name := range d.stamps {
		if _, here := found[name]; here {
			continue
		}
		delete(d.stamps, name)
		if _, had := d.values[name]; had {
			delete(d.values, name)
			changed = true
		}
	}
	return changed
}

// load reads and parses one file and reports whether it now has a new value.
func (d *Dir[T]) load(name string) bool {
	data, err := d.read(filepath.Join(d.dir, name))
	var value T
	if err == nil {
		value, err = d.parse(name, data)
	}
	if err != nil {
		d.log.Warn("ignoring file, keeping its last good content",
			slog.String("dir", d.dir), slog.String("file", name), slog.Any("error", err))
		return false
	}
	d.values[name] = value
	return true
}

// sorted returns the loaded values in file name order.
func (d *Dir[T]) sorted() []Entry[T] {
	names := make([]string, 0, len(d.values))
	for name := range d.values {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([]Entry[T], len(names))
	for i, name := range names {
		entries[i] = Entry[T]{Name: name, Value: d.values[name], Stamp: d.stamps[name]}
	}
	return entries
}
