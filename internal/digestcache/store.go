package digestcache

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"

	"github.com/SpliceFS/server/internal/filestat"
)

const (
	// tempFolder holds the entries being written. Share names cannot start with a dot, so
	// it is never the folder of a share.
	tempFolder = ".tmp"
	folderMode = 0o700
	fileMode   = 0o600
)

var errNotRegular = errors.New("is not a regular file")

// Store keeps the entries in the digest folder, local/digests of the state folder. Every
// access goes through an os.Root on that folder, so a link placed inside it cannot send a
// read or a write elsewhere. A Store is safe for concurrent use.
type Store struct {
	root *os.Root
}

// Open starts the store on the digest folder dir, which must exist. It empties the temporary
// folder first: the server is the only writer and has not started writing yet, so what is
// there was left by a crash. Close releases the folder.
func Open(dir string) (*Store, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open the digest folder: %w", err)
	}
	store, err := start(root)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return store, nil
}

// Close releases the digest folder. The store must not be used after it.
func (s *Store) Close() error {
	return s.root.Close() //nolint:wrapcheck // the caller says it was closing the digest folder
}

// start starts the store on the digest folder open as root.
func start(root *os.Root) (*Store, error) {
	// After a successful removal only a folder that cannot be written fails the creation.
	err := errors.Join(root.RemoveAll(tempFolder), root.Mkdir(tempFolder, folderMode))
	if err != nil {
		return nil, fmt.Errorf("empty the temporary folder of the digest entries: %w", err)
	}
	return &Store{root: root}, nil
}

// Load returns the digests of the file at filePath in share, which has stamp now, when its
// entry was saved for that path and that stamp. A path that is not clean, and an entry that
// is missing, unreadable, not a regular file, not exactly what the server writes or of
// another path or stamp, is a miss: the caller computes the digests again and saves them.
func (s *Store) Load(share, filePath string, stamp filestat.Stamp) (Digests, bool) {
	name, err := entryPath(filePath)
	if err != nil {
		return Digests{}, false
	}
	data, err := s.read(share + "/" + name)
	if err != nil {
		return Digests{}, false
	}
	found, err := decode(data)
	if err != nil || found.Key != KeyOf(filePath, stamp) {
		return Digests{}, false
	}
	return Digests{Head: found.Head, Tail: found.Tail}, true
}

// Save writes the entry of the file at filePath in share, which had stamp when digests were
// read from it, replacing the entry there. It goes to a file of its own in the temporary
// folder first and is renamed into place, so a reader sees the old entry or the new one,
// never a part of one, and two writers of one entry never write into the same file. The
// entry is not flushed: a lost entry costs one recompute.
func (s *Store) Save(share, filePath string, stamp filestat.Stamp, digests Digests) error {
	name, err := entryPath(filePath)
	if err != nil {
		return err
	}
	name = share + "/" + name
	err = s.root.MkdirAll(path.Dir(name), folderMode)
	if err != nil {
		return fmt.Errorf("make the folder of digest entry %s: %w", name, err)
	}
	temp := tempFolder + "/" + rand.Text()
	err = s.root.WriteFile(temp, newEntry(filePath, stamp, digests).encode(), fileMode)
	if err == nil {
		err = s.root.Rename(temp, name)
	}
	if err != nil {
		return fmt.Errorf("save digest entry %s: %w", name, errors.Join(err, s.removeTemp(temp)))
	}
	return nil
}

// removeTemp removes the temporary file of a failed save, which a failed creation never made.
func (s *Store) removeTemp(temp string) error {
	err := s.root.Remove(temp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err //nolint:wrapcheck // joined to the error of the save, which names the entry
}

// read returns the content of the regular file name. It opens without blocking, because
// opening a FIFO for reading would otherwise wait for a writer.
func (s *Store) read(name string) ([]byte, error) {
	file, err := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open digest entry: %w", err)
	}
	defer file.Close() //nolint:errcheck // the file was only read, so a failed close loses nothing
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("digest entry %s %w", name, errNotRegular)
	}
	return io.ReadAll(file) //nolint:wrapcheck // a failed read is a miss, the caller never shows it
}
