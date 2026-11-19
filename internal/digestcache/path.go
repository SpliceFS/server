package digestcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// stateSuffix ends the file of every entry and no folder, which keeps a file and a
	// folder of the same name apart.
	stateSuffix = ".splicefs_state"
	// maxNameBytes is the longest name the file systems the server runs on take.
	maxNameBytes = 255
	// hashedPrefix starts a name that was too long. It cannot come from escaping a name,
	// which writes a percent sign only before two hex digits.
	hashedPrefix = "%_"
	upperHex     = "0123456789ABCDEF"
)

var (
	errNotClean   = errors.New("is not a clean path inside a share")
	errNotEscaped = errors.New("is not what escaping a path gives")
)

// escape returns path as an entry stores it: every name of the path, split at slashes, with
// rules 1 to 3 of the design applied. They can be undone, so two paths never share an
// escaped form, and the result is valid UTF-8 whatever bytes the names held.
func escape(path string) string {
	names := strings.Split(path, "/")
	for i, name := range names {
		names[i] = escapeName(name)
	}
	return strings.Join(names, "/")
}

// entryPath returns where the entry of the file at path is, relative to the folder of its
// share in the digest folder: every name escaped by rules 1 to 4 of the design, and the
// suffix after the last one. path must be a clean path inside the share, names separated by
// slashes, none empty, none "." or "..": any other path fails, because it could put an entry
// outside the folder of its share or give one file two entries. Two different paths give two
// different entry paths, so no two files share an entry, unless two names have the same
// SHA-256 sum.
func entryPath(path string) (string, error) {
	names := strings.Split(path, "/")
	for i, name := range names {
		if name == "" || name == "." || name == ".." {
			return "", fmt.Errorf("digest entry of %q: %w", path, errNotClean)
		}
		names[i] = entryName(name)
	}
	return strings.Join(names, "/") + stateSuffix, nil
}

// entryName escapes one name by rules 1 to 4: the escaped form, unless that form with the
// suffix would be longer than a file system takes, and then the SHA-256 sum of the name.
func entryName(name string) string {
	escaped := escapeName(name)
	if len(escaped)+len(stateSuffix) <= maxNameBytes {
		return escaped
	}
	sum := sha256.Sum256([]byte(name))
	return hashedPrefix + hex.EncodeToString(sum[:])
}

// escapeName applies rules 1 to 3 of the design to one name. Rule 3 is for a name that ends
// in the suffix, which would otherwise look like an entry.
func escapeName(name string) string {
	var escaped strings.Builder
	for i := 0; i < len(name); {
		char, size := utf8.DecodeRuneInString(name[i:])
		switch {
		case char == '%':
			escaped.WriteString("%25")
		case char == utf8.RuneError && size == 1:
			escaped.WriteByte('%')
			escaped.WriteByte(upperHex[name[i]>>4])
			escaped.WriteByte(upperHex[name[i]&0x0f])
		default:
			escaped.WriteString(name[i : i+size])
		}
		i += size
	}
	if base, found := strings.CutSuffix(escaped.String(), stateSuffix); found {
		return base + "%2E" + stateSuffix[1:]
	}
	return escaped.String()
}

// unescape undoes escape. It fails for a path escape cannot have made: a percent sign that is
// not followed by two upper case hex digits.
func unescape(escaped string) (string, error) {
	const escapedByte = len("%XX")
	var path strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '%' {
			path.WriteByte(escaped[i])
			continue
		}
		digits := escaped[i+1 : min(i+escapedByte, len(escaped))]
		value, err := hex.DecodeString(digits)
		if err != nil || len(value) != 1 || strings.ToUpper(digits) != digits {
			return "", fmt.Errorf("escaped path %q: %w", escaped, errNotEscaped)
		}
		path.Write(value)
		i += escapedByte - 1
	}
	return path.String(), nil
}
