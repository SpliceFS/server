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
)

// Entry is the content of one file of a directory.
type Entry[T any] struct {
	// Name is the file name, with its extension.
	Name  string
	Value T
}

// stamp is what tells that a file changed: the same two values the server has always used
// for its single files, so that no file is read twice for nothing.
type stamp struct {
	size    int64
	modTime time.Time
}

// Dir follows a directory of YAML files. Each use lists the directory, compares names, sizes
// and modification times with what it has loaded and reads only the files that changed, so
// no signal is needed when another program changes the directory. It is not safe for
// concurrent use; the caller serializes calls.
//
// A file that cannot be read or parsed is ignored with a warning and the last good content
// of that file stays in use, so one bad file never affects another and never stops the
// server. Files whose name does not end in ".yaml", or starts with a dot (sync tools leave
// temporary files like that), are not files of the directory.
type Dir[T any] struct {
	dir    string
	parse  func(name string, data []byte) (T, error)
	log    *slog.Logger
	read   func(path string) ([]byte, error)
	stamps map[string]stamp
	values map[string]T
}

// NewDir returns a Dir for dir. parse turns the content of the named file into a value; its
// error text is logged, so it must never contain content that is secret.
func NewDir[T any](
	dir string, parse func(name string, data []byte) (T, error), log *slog.Logger,
) *Dir[T] {
	return &Dir[T]{
		dir: dir, parse: parse, log: log, read: os.ReadFile,
		stamps: map[string]stamp{}, values: map[string]T{},
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
func (d *Dir[T]) scan() (map[string]stamp, bool) {
	items, err := os.ReadDir(d.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.log.Warn("cannot list directory, keeping what was loaded",
			slog.String("dir", d.dir), slog.Any("error", err))
		return nil, false
	}
	found := map[string]stamp{}
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
		found[name] = stamp{size: info.Size(), modTime: info.ModTime()}
	}
	return found, true
}

// drop forgets the files that are gone and reports whether any had a value.
func (d *Dir[T]) drop(found map[string]stamp) bool {
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
		entries[i] = Entry[T]{Name: name, Value: d.values[name]}
	}
	return entries
}
