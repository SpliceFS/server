// Package uuid7 makes and checks UUIDv7 identifiers as RFC 9562 defines them: 48 bits of Unix
// time in milliseconds, then random bits, so ids made later sort after ids made earlier.
package uuid7

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// size is the number of bytes in an id, timeSize of them the timestamp.
	size     = 16
	timeSize = 6
	// versionByte and variantByte are the bytes of an id that carry the version in their
	// high nibble and the variant in their two high bits. The other bits stay random.
	versionByte = 6
	variantByte = 8
	lowNibble   = 0x0f
	lowSixBits  = 0x3f
	versionBits = 0x70
	variantBits = 0x80
	// variantDigits are the digits that show variant 10.
	variantDigits = "89ab"
	// layout is the text form: x is a lowercase hexadecimal digit, 7 the version, y a digit
	// of the variant. Valid reads its input against it, so the form is stated once.
	layout = "xxxxxxxx-xxxx-7xxx-yxxx-xxxxxxxxxxxx"
)

// New returns the id for the time now in its canonical form: 36 lowercase characters in
// five groups separated by hyphens. The bits that are not the timestamp, the version or
// the variant come from random.
func New(now time.Time, random io.Reader) (string, error) {
	var raw [size]byte
	if _, err := io.ReadFull(random, raw[timeSize:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	// The timestamp is the low 48 bits of the milliseconds, written big endian so that ids
	// sort in time order.
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(now.UnixMilli()))
	copy(raw[:timeSize], stamp[len(stamp)-timeSize:])
	raw[versionByte] = raw[versionByte]&lowNibble | versionBits
	raw[variantByte] = raw[variantByte]&lowSixBits | variantBits
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

// Valid reports whether s is a UUIDv7 in exactly the form New returns: lowercase, with the
// hyphens, version 7 and variant 10.
func Valid(s string) bool {
	if len(s) != len(layout) {
		return false
	}
	for i := range len(s) {
		if !matches(layout[i], s[i]) {
			return false
		}
	}
	return true
}

// matches reports whether the character got may stand where the layout has want.
func matches(want, got byte) bool {
	switch want {
	case 'y':
		return strings.IndexByte(variantDigits, got) >= 0
	case 'x':
		return got >= '0' && got <= '9' || got >= 'a' && got <= 'f'
	default:
		return got == want
	}
}
