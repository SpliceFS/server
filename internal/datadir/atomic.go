// Package datadir holds what the data under the state directory needs from the file
// system: writing a file so that no reader sees half of it, and following a directory of
// YAML files that other programs, such as sync tools, change at any time.
package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// tempPrefix starts with a dot so that Dir ignores a temporary file left by a crash.
const tempPrefix = ".tmp-"

// WriteAtomic replaces the file at path with data and gives it mode. The data goes to a
// temporary file in the same folder first and is renamed over path, so a reader (or a sync
// tool) sees either the old file or the new one, never a partial one. The folder must exist.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPrefix+"*")
	if err != nil {
		return err
	}
	err = writeTemp(tmp, data, mode)
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		err = errors.Join(err, os.Remove(tmp.Name()))
	}
	return err
}

// writeTemp fills and closes the temporary file. The mode is set explicitly because
// CreateTemp always uses 0600 and the umask would change a mode given at creation.
func writeTemp(f *os.File, data []byte, mode os.FileMode) error {
	_, writeErr := f.Write(data)
	return errors.Join(f.Chmod(mode), writeErr, f.Sync(), f.Close())
}

// IsNodeID reports whether s is a node id: 16 lowercase hexadecimal characters.
func IsNodeID(s string) bool {
	if len(s) != nodeIDLength {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// nodeIDLength is the length of a node id.
const nodeIDLength = 16

// FileExt is the extension of the files that Dir reads.
const FileExt = ".yaml"

// NodeFile is the name of the file that the node with the given id writes in a shared
// directory.
func NodeFile(id string) string {
	return id + FileExt
}

// NodeOfFile returns the node id in a file name of the form "<node id>.yaml".
func NodeOfFile(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, FileExt)
	return id, ok && IsNodeID(id)
}
