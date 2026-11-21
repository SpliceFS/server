package cluster

import (
	"log/slog"
	"slices"
	"sync"

	"github.com/SpliceFS/server/internal/datadir"
)

// KeyReader reads the cluster key from the cluster folder. It is safe for concurrent use.
type KeyReader struct {
	mu    sync.Mutex
	files *datadir.Dir[Key]
}

// NewKeyReader returns a KeyReader for the cluster folder dir. A file that cannot be used is
// logged to log.
func NewKeyReader(dir string, log *slog.Logger) *KeyReader {
	return &KeyReader{files: datadir.NewDir(dir, parseKeyFile, log)}
}

// Current returns the cluster key, and whether there is one. The folder is read again first,
// so a key that appears, changes or goes takes effect at once. A file that cannot be read or
// parsed is ignored with a warning, and the last good key stays in use; a cluster with no good
// key has none. The result does not share memory with the reader.
func (r *KeyReader) Current() (Key, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, _ := r.files.Refresh()
	// Only the key file parses, so a folder holds at most one value.
	if len(entries) == 0 {
		return Key{}, false
	}
	return entries[0].Value.clone(), true
}

// parseKeyFile is the parse step of the folder: the file must be the key file, so that a stray
// file never becomes the key of the cluster.
func parseKeyFile(name string, data []byte) (Key, error) {
	if name != KeyFile {
		return Key{}, errNotKeyFile
	}
	return ParseKey(data)
}

// clone returns the key without sharing the memory of the private key.
func (k Key) clone() Key {
	return Key{ID: k.ID, Private: slices.Clone(k.Private)}
}
