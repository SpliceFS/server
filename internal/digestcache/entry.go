// Package digestcache keeps the head and tail digests of the data files a node has been
// asked about in files of the node's state folder, so that a restart or a reload does not
// read the files again. See "Digest cache" in docs/design.md.
package digestcache

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/server/internal/filestat"
)

// version is what an entry means. It changes whenever that changes, for example the 64 KiB
// the digests cover; an entry of another version is stale, and decode rejects it.
const version = 1

var (
	errVersion       = errors.New("not the version this server writes")
	errSize          = errors.New("size is negative")
	errDigest        = errors.New("is not 64 lower case hex digits")
	errNotWhatIsMade = errors.New("is not what the server writes for the fields it holds")
)

// entry is what a node knows about one data file: the stamp the file had when its digests
// were read, and the digests. Times are in nanoseconds since the Unix epoch, as in
// filestat.Stamp. The device number is not part of an entry: ZFS, btrfs and network file
// systems give a file system another device number after a reboot, which would make every
// entry stale.
type entry struct {
	// Path is the path of the file in its share, escaped by escape.
	Path    string `yaml:"path"`
	Size    int64  `yaml:"size"`
	MtimeNs int64  `yaml:"mtime_ns"`
	CtimeNs int64  `yaml:"ctime_ns"`
	// BtimeNs is 0 where the file system or the system does not report a creation time.
	BtimeNs int64  `yaml:"btime_ns"`
	Inode   uint64 `yaml:"inode"`
	// Head and Tail are the hex SHA-256 digests of the first and the last 64 KiB of the file.
	Head string `yaml:"head"`
	Tail string `yaml:"tail"`
}

// Digests are the hex SHA-256 digests of the first and the last 64 KiB of one version of a
// file. They are kept together because one read of the file gives both.
type Digests struct {
	Head string
	Tail string
}

// newEntry returns the entry of the file at path in its share, which had stamp when digests
// were read from it.
func newEntry(path string, stamp filestat.Stamp, digests Digests) entry {
	return entry{
		Path:    escape(path),
		Size:    stamp.Size,
		MtimeNs: stamp.Mtime,
		CtimeNs: stamp.Ctime,
		BtimeNs: stamp.Btime,
		Inode:   stamp.Ino,
		Head:    digests.Head,
		Tail:    digests.Tail,
	}
}

// document is an entry as it is written: the entry behind its version. The version is the
// constant version and not a field of entry, so that no entry can carry another one.
type document struct {
	Version int   `yaml:"version"`
	Entry   entry `yaml:",inline"`
}

// encode returns the YAML file of the entry, with the fields in the order of the design.
func (e entry) encode() []byte {
	// A struct of strings and numbers always encodes, so there is no error to handle.
	data, _ := yaml.Marshal(document{Version: version, Entry: e}) //nolint:errcheck // cannot fail for strings and numbers
	return data
}

// decode reads the YAML file of an entry. It accepts only what encode writes: another
// version, a negative size, a digest that is not 64 lower case hex digits and any other
// difference make it fail. The caller treats that as a missing entry and computes the
// digests again.
func decode(data []byte) (entry, error) {
	var doc document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return entry{}, fmt.Errorf("parse digest entry: %w", err)
	}
	if err := doc.validate(); err != nil {
		return entry{}, err
	}
	// The server is the only writer, so a file is right when it is, byte for byte, what
	// encode makes of the fields read from it. That rejects what the YAML decoder reads
	// leniently: a number with a fraction or too large for its field, a field that is
	// unknown or missing, a second document.
	if !bytes.Equal(doc.Entry.encode(), data) {
		return entry{}, fmt.Errorf("digest entry %w", errNotWhatIsMade)
	}
	return doc.Entry, nil
}

// validate checks the fields of a document that the YAML types cannot.
func (d document) validate() error {
	switch {
	case d.Version != version:
		return fmt.Errorf("digest entry version %d: %w", d.Version, errVersion)
	case d.Entry.Size < 0:
		return fmt.Errorf("digest entry: %w", errSize)
	case !isDigest(d.Entry.Head):
		return fmt.Errorf("digest entry head %q %w", d.Entry.Head, errDigest)
	case !isDigest(d.Entry.Tail):
		return fmt.Errorf("digest entry tail %q %w", d.Entry.Tail, errDigest)
	}
	return nil
}

// isDigest says whether text is what hex.EncodeToString makes of a SHA-256 sum, which is
// also what a client compares: lower case only.
func isDigest(text string) bool {
	return len(text) == 2*sha256.Size && strings.Trim(text, "0123456789abcdef") == ""
}

// match says whether the entry is still right for the file at path in its share, which
// has the given stamp. path is escaped, as entry.Path is. Every field the entry holds must
// be equal to what the file system reports now; the device number and the mode are not
// compared, because the entry does not hold them.
func (e entry) match(path string, stamp filestat.Stamp) bool {
	return e.Path == path &&
		e.Size == stamp.Size &&
		e.MtimeNs == stamp.Mtime &&
		e.CtimeNs == stamp.Ctime &&
		e.BtimeNs == stamp.Btime &&
		e.Inode == stamp.Ino
}
