package share

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// resolved is a request name that passed every check, with what the file system
// reports for it.
type resolved struct {
	// relPath is the path below the share root, "." for the root itself.
	relPath string
	info    os.FileInfo
}

// lookup resolves a request name to a visible file or directory. Every segment of
// the path is checked against the exclude patterns and the symbolic link rules, so
// a hidden directory hides everything below it.
func (s *Share) lookup(name string) (resolved, error) {
	relPath, ok := cleanName(name)
	if !ok {
		return resolved{}, ErrNotFound
	}
	if relPath == "." {
		fileInfo, err := s.statEntry(relPath)
		return resolved{relPath: relPath, info: fileInfo}, err
	}
	segments := strings.Split(relPath, "/")
	var fileInfo os.FileInfo
	for i, segment := range segments {
		if s.excluded(segment) {
			return resolved{}, ErrNotFound
		}
		var err error
		if fileInfo, err = s.statEntry(strings.Join(segments[:i+1], "/")); err != nil {
			// A client chooses the name, and a name longer than the file system allows
			// cannot exist, so it is a miss and not a server error.
			if errors.Is(err, syscall.ENAMETOOLONG) {
				err = ErrNotFound
			}
			return resolved{}, err
		}
		if i < len(segments)-1 && !fileInfo.IsDir() {
			return resolved{}, ErrNotFound
		}
	}
	if !s.visible(fileInfo) {
		return resolved{}, ErrNotFound
	}
	return resolved{relPath: relPath, info: fileInfo}, nil
}

// statEntry describes the entry relPath, a validated path below the root. A symbolic
// link is hidden unless links are followed and its target resolves inside the root.
func (s *Share) statEntry(relPath string) (os.FileInfo, error) {
	fileInfo, err := s.root.Lstat(relPath)
	if err != nil {
		return nil, s.fail(relPath, err)
	}
	if fileInfo.Mode()&fs.ModeSymlink == 0 {
		return fileInfo, nil
	}
	if !s.follow {
		return nil, ErrNotFound
	}
	// A link that cannot be resolved inside the root (it escapes, dangles or loops)
	// is hidden, whatever the reason.
	if fileInfo, err = s.root.Stat(relPath); err != nil {
		return nil, ErrNotFound
	}
	return fileInfo, nil
}

// fail turns a file system error for path relPath into ErrNotFound when the thing is
// absent or unreachable from inside the share, and into a wrapped error otherwise.
func (s *Share) fail(relPath string, err error) error {
	if absent(err) {
		return ErrNotFound
	}
	return fmt.Errorf("share %q: %s: %w", s.name, relPath, err)
}

// absent reports whether err means the path does not exist inside the share.
func absent(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return true
	}
	// os.Root reports a path that leaves the root with an unexported error value.
	return strings.Contains(err.Error(), "path escapes from parent")
}
