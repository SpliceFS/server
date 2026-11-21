package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/SpliceFS/server/internal/datadir"
)

// localValue is a value that this node makes on its first start and keeps for good in a file of
// the local folder: the node id and the node keys. The file is never replaced, because a new
// value would be a different node.
type localValue[T any] struct {
	// what names the value in the errors.
	what string
	path string
	mode os.FileMode
	// parse checks the content of the file and returns the value; its error says what is wrong.
	parse func(data []byte) (T, error)
	// create returns the content of a new file.
	create func() ([]byte, error)
}

// load returns the value of the file, making the file first when it is missing. A file that is
// there but malformed is an error.
func (v localValue[T]) load() (T, error) { //nolint:ireturn // T is the type the caller chose
	var none T
	if err := v.ensure(); err != nil {
		return none, err
	}
	data, err := os.ReadFile(v.path)
	if err != nil {
		return none, fmt.Errorf("read %s file: %w", v.what, err)
	}
	value, err := v.parse(data)
	if err != nil {
		return none, fmt.Errorf("%s file %s %w", v.what, v.path, err)
	}
	return value, nil
}

// ensure saves a new file when there is none. When another start saved its file first, that
// file is the one the node has and the new content is dropped, so the check and the write
// need no lock. The next read finds the file whichever start made it.
func (v localValue[T]) ensure() error {
	if _, err := os.Stat(v.path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	data, err := v.create()
	if err != nil {
		return fmt.Errorf("make %s: %w", v.what, err)
	}
	if err = datadir.WriteOnce(v.path, data, v.mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("write %s file %s: %w", v.what, v.path, err)
	}
	return nil
}
