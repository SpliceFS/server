package digestcache

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sync"
	"sync/atomic"
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

// ErrNotAFolder is the cause of a Reopen that fails because a file sits where a folder goes.
var ErrNotAFolder = errors.New("is not a folder")

// Store keeps the entries in the digest folder, local/digests of the state folder. Every
// access goes through an os.Root on that folder, so a link placed inside it cannot send a
// read or a write elsewhere. A Store is safe for concurrent use.
type Store struct {
	dir string
	// root is the digest folder that every access goes through. Reopen replaces it.
	root   atomic.Pointer[os.Root]
	logger *slog.Logger
	// mu serializes Reopen and Close, which change retired.
	mu sync.Mutex
	// retired are the folders Reopen replaced. A request may still be using one, so they are
	// closed with the store. There is one for each time the folder was removed.
	retired []*os.Root
	// failing is true from a failed Keep to the next one that works. The folder is the node's,
	// so a full or read-only one is logged once for all shares, not once per share or request.
	failing atomic.Bool
}

// Open starts the store on the digest folder dir, which must exist. It empties the temporary
// folder first: the server is the only writer and has not started writing yet, so what is
// there was left by a crash. logger receives the warning of Keep. Close releases the folder.
func Open(dir string, logger *slog.Logger) (*Store, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open the digest folder: %w", err)
	}
	store, err := start(dir, root, logger)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return store, nil
}

// Close releases the digest folder. The store must not be used after it.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	errs := make([]error, 0, 1+len(s.retired))
	errs = append(errs, s.root.Load().Close())
	for _, root := range s.retired {
		errs = append(errs, root.Close())
	}
	return errors.Join(errs...)
}

// start starts the store on the digest folder dir, open as root.
func start(dir string, root *os.Root, logger *slog.Logger) (*Store, error) {
	// After a successful removal only a folder that cannot be written fails the creation.
	err := errors.Join(root.RemoveAll(tempFolder), root.Mkdir(tempFolder, folderMode))
	if err != nil {
		return nil, fmt.Errorf("empty the temporary folder of the digest entries: %w", err)
	}
	store := &Store{dir: dir, logger: logger}
	store.root.Store(root)
	return store, nil
}

// Reopen heals the store after its folders were removed while it ran: it opens the digest
// folder again when the one at its path is not the one open, and makes the temporary folder
// when it is missing. What is in the temporary folder stays, since requests may be writing
// there.
func (s *Store) Reopen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := s.root.Load()
	same, err := isFolder(root, s.dir)
	if err != nil {
		return err
	}
	if !same {
		root, err = os.OpenRoot(s.dir)
		if err != nil {
			return fmt.Errorf("open the digest folder again: %w", err)
		}
		s.retired = append(s.retired, s.root.Swap(root))
	}
	// It usually exists. Whatever kept it from being made shows in the check after.
	mkdirErr := root.Mkdir(tempFolder, folderMode)
	info, err := root.Stat(tempFolder)
	if err == nil && !info.IsDir() {
		err = ErrNotAFolder
	}
	if err != nil {
		return fmt.Errorf("make the temporary folder of the digest entries: %w", errors.Join(mkdirErr, err))
	}
	return nil
}

// isFolder reports whether root is the folder at dir. A root whose folder was removed is not.
func isFolder(root *os.Root, dir string) (bool, error) {
	want, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("find the digest folder: %w", err)
	}
	have, err := root.Stat(".")
	return err == nil && os.SameFile(have, want), nil
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
	// One folder for the whole save, even if Reopen replaces it meanwhile.
	root := s.root.Load()
	err = root.MkdirAll(path.Dir(name), folderMode)
	if err != nil {
		return fmt.Errorf("make the folder of digest entry %s: %w", name, err)
	}
	temp := tempFolder + "/" + rand.Text()
	err = root.WriteFile(temp, newEntry(filePath, stamp, digests).encode(), fileMode)
	if err == nil {
		err = root.Rename(temp, name)
	}
	if err != nil {
		return fmt.Errorf("save digest entry %s: %w", name, errors.Join(err, removeTemp(root, temp)))
	}
	return nil
}

// Keep saves the entry like Save. A failed save costs a read after the next restart, not the
// request, so it is only logged: once, and again only after a save has worked in between.
func (s *Store) Keep(share, filePath string, stamp filestat.Stamp, digests Digests) {
	err := s.Save(share, filePath, stamp, digests)
	if err == nil {
		s.failing.Store(false)
		return
	}
	if !s.failing.Swap(true) {
		s.logger.Warn("cannot store digests, they will be read again after a restart",
			slog.String("share", share), slog.Any("error", err))
	}
}

// removeTemp removes the temporary file of a failed save, which a failed creation never made.
func removeTemp(root *os.Root, temp string) error {
	err := root.Remove(temp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err //nolint:wrapcheck // joined to the error of the save, which names the entry
}

// read returns the content of the regular file name. It opens without blocking, because
// opening a FIFO for reading would otherwise wait for a writer.
func (s *Store) read(name string) ([]byte, error) {
	file, err := s.root.Load().OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open digest entry: %w", err)
	}
	defer file.Close() //nolint:errcheck // the file was only read, so a failed close loses nothing
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("digest entry %s %w", name, errNotRegular)
	}
	return io.ReadAll(file) //nolint:wrapcheck // a failed read is a miss, the caller never shows it
}
