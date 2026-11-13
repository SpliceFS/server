package share

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/SpliceFS/common/wire"
)

// File is a regular file opened for reading. It implements io.ReadSeeker and
// io.Closer.
type File struct {
	f    *os.File
	info Info
}

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) {
	return f.f.Read(p) //nolint:wrapcheck // io.Reader callers compare the error with io.EOF, so it must not be wrapped
}

// Seek implements io.Seeker.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	position, err := f.f.Seek(offset, whence)
	if err != nil {
		return 0, fmt.Errorf("share file %q: seek: %w", f.info.Name, err)
	}
	return position, nil
}

// Close implements io.Closer.
func (f *File) Close() error {
	if err := f.f.Close(); err != nil {
		return fmt.Errorf("share file %q: close: %w", f.info.Name, err)
	}
	return nil
}

// Info describes the file that was opened.
func (f *File) Info() Info {
	return f.info
}

// ETag returns the strong entity tag for a file of the given size and modification
// time: "<size hex>-<unix seconds hex>-<nanoseconds hex>", quotes included. The time
// is used as given, so callers truncate it first.
func ETag(size int64, mtime time.Time) string {
	return fmt.Sprintf(`"%x-%x-%x"`, size, mtime.Unix(), mtime.Nanosecond())
}

// headDigest returns the hex SHA-256 of the first wire.HeadDigestBytes bytes of r.
func headDigest(r io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(r, wire.HeadDigestBytes)); err != nil {
		return "", fmt.Errorf("hash head: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
