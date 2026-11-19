package digestcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"

	"github.com/SpliceFS/server/internal/filestat"
)

// Files answers for the files of one share what a request would find.
type Files interface {
	// Stamp returns the stamp of the file at path, and false when a request would not find it
	// because it is gone, hidden or not a regular file. An error means it could not tell.
	Stamp(path string) (stamp filestat.Stamp, found bool, err error)
}

// Sweep removes the entries that no request can use any more: of a file that is gone, hidden
// or changed, anything in a share folder that is not an entry the server wrote there, the
// folders this leaves empty and the folders of shares that are not in shares. It keeps the
// entries of current files and the temporary folder. A failure on one entry is logged and
// the sweep goes on. It stops when ctx is done and then returns ctx.Err(). It fails when the
// digest folder cannot be listed, for example after Close.
//
// A sweep can remove an entry or a folder that a request has just written; that costs one
// recompute, never a wrong answer.
func (s *Store) Sweep(ctx context.Context, shares map[string]Files) error {
	root := s.root.Load()
	found, err := readFolder(root, ".")
	if err != nil {
		return fmt.Errorf("list the digest folder: %w", err)
	}
	for _, item := range found {
		name := item.Name()
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller compares it with context.Canceled
		}
		if name == tempFolder {
			continue
		}
		files, configured := shares[name]
		if !configured {
			s.logFailure(root.RemoveAll(name), name)
			continue
		}
		if err := s.sweepShare(ctx, root, name, files); err != nil {
			return err
		}
	}
	return nil
}

// sweepShare sweeps the folder of one share, depth first with a stack of folders to visit,
// and removes the folders it leaves empty, deepest first. A folder it cannot read is logged
// and left as it is.
func (s *Store) sweepShare(ctx context.Context, root *os.Root, share string, files Files) error {
	pending := []string{share}
	var visited []string
	for len(pending) > 0 {
		folder := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := readFolder(root, folder)
		if err != nil {
			s.logFailure(err, folder)
			continue
		}
		visited = append(visited, folder)
		for _, found := range entries {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // the caller compares it with context.Canceled
			}
			name := folder + "/" + found.Name()
			if found.IsDir() {
				pending = append(pending, name)
				continue
			}
			s.sweepEntry(root, share, name, files)
		}
	}
	slices.Reverse(visited)
	for _, folder := range visited {
		// Only an empty folder can be removed; one that still holds entries stays.
		_ = root.Remove(folder) //nolint:errcheck // a folder that is not empty is meant to stay
	}
	return nil
}

// sweepEntry removes the file name of the folder of share unless it is the entry, at its
// place, of a file that a request finds with the stamp the entry holds.
func (s *Store) sweepEntry(root *os.Root, share, name string, files Files) {
	path, held, ok := placedEntry(root, share, name)
	if !ok {
		s.logFailure(root.Remove(name), name)
		return
	}
	stamp, found, err := files.Stamp(path)
	if err != nil {
		s.logFailure(err, name)
		return
	}
	if !found || KeyOf(path, stamp) != held {
		s.logFailure(root.Remove(name), name)
	}
}

// placedEntry returns the path of the file whose entry name is and the key the entry holds,
// when name is an entry the server wrote and lies where the entry of that path goes.
func placedEntry(root *os.Root, share, name string) (string, Key, bool) {
	data, err := read(root, name)
	if err != nil {
		return "", Key{}, false
	}
	found, err := decode(data)
	if err != nil {
		return "", Key{}, false
	}
	path, err := unescape(found.Path)
	if err != nil {
		return "", Key{}, false
	}
	place, err := entryPath(path)
	return path, found.Key, err == nil && share+"/"+place == name
}

// logFailure logs a failure to sweep name, if there was one.
func (s *Store) logFailure(err error, name string) {
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logger.Warn("cannot sweep digest entry", slog.String("entry", name), slog.Any("error", err))
	}
}

// readFolder returns what is in folder. A link is not a folder, so the sweep never follows one.
func readFolder(root *os.Root, folder string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(root.FS(), folder)
	return entries, err //nolint:wrapcheck // the caller names the folder
}
