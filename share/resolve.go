package share

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// lookup resolves a request name to a visible file or directory. Every segment of
// the path is checked against the exclude patterns and the symbolic link rules, so
// a hidden directory hides everything below it.
func (s *Share) lookup(name string) (string, os.FileInfo, error) {
	p, ok := cleanName(name)
	if !ok {
		return "", nil, ErrNotFound
	}
	if p == "." {
		fi, err := s.statEntry(p)
		return p, fi, err
	}
	segments := strings.Split(p, "/")
	var fi os.FileInfo
	for i, segment := range segments {
		if s.excluded(segment) {
			return "", nil, ErrNotFound
		}
		var err error
		if fi, err = s.statEntry(strings.Join(segments[:i+1], "/")); err != nil {
			return "", nil, err
		}
		if i < len(segments)-1 && !fi.IsDir() {
			return "", nil, ErrNotFound
		}
	}
	if !s.visible(fi) {
		return "", nil, ErrNotFound
	}
	return p, fi, nil
}

// statEntry describes the entry p, a validated path below the root. A symbolic link
// is hidden unless links are followed and its target resolves inside the root.
func (s *Share) statEntry(p string) (os.FileInfo, error) {
	fi, err := s.root.Lstat(p)
	if err != nil {
		return nil, s.fail(p, err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return fi, nil
	}
	if !s.follow {
		return nil, ErrNotFound
	}
	// A link that cannot be resolved inside the root (it escapes, dangles or loops)
	// is hidden, whatever the reason.
	if fi, err = s.root.Stat(p); err != nil {
		return nil, ErrNotFound
	}
	return fi, nil
}

// fail turns a file system error for path p into ErrNotFound when the thing is
// absent or unreachable from inside the share, and into a wrapped error otherwise.
func (s *Share) fail(p string, err error) error {
	if absent(err) {
		return ErrNotFound
	}
	return fmt.Errorf("share %q: %s: %w", s.name, p, err)
}

// absent reports whether err means the path does not exist inside the share.
func absent(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return true
	}
	// os.Root reports a path that leaves the root with an unexported error value.
	return strings.Contains(err.Error(), "path escapes from parent")
}
