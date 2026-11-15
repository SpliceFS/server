package uuid7

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// rfcTime is the time of the UUIDv7 example in RFC 9562, appendix A.6.
func rfcTime() time.Time { return time.UnixMilli(0x017F22E279B0) }

// rfcRandom holds the random bytes that give the RFC example. The high nibble of the first
// byte and the two high bits of the third are set to the opposite of the version and
// variant, so the vector also proves that those bits are overwritten.
func rfcRandom() io.Reader {
	return bytes.NewReader([]byte{0xFC, 0xC3, 0xD8, 0xC4, 0xDC, 0x0C, 0x0C, 0x07, 0x39, 0x8F})
}

func TestNewVector(t *testing.T) {
	t.Parallel()
	got, err := New(rfcTime(), rfcRandom())
	if err != nil {
		t.Fatal(err)
	}
	if want := "017f22e2-79b0-7cc3-98c4-dc0c0c07398f"; got != want {
		t.Errorf("New = %q, want %q", got, want)
	}
}

func TestNewSetsVersionAndVariant(t *testing.T) {
	t.Parallel()
	for _, fill := range []byte{0x00, 0xFF} {
		id, err := New(rfcTime(), bytes.NewReader(bytes.Repeat([]byte{fill}, 10)))
		if err != nil {
			t.Fatal(err)
		}
		if id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Errorf("random bytes %#x: version and variant of %q are wrong", fill, id)
		}
		if !Valid(id) {
			t.Errorf("New gave %q, which Valid refuses", id)
		}
	}
}

func TestNewOrdersByTime(t *testing.T) {
	t.Parallel()
	var previous string
	for _, millis := range []int64{0, 1, 255, 256, 1 << 32, 0x017F22E279B0, 0x017F22E279B1, 1<<48 - 1} {
		id, err := New(time.UnixMilli(millis), bytes.NewReader(make([]byte, 10)))
		if err != nil {
			t.Fatal(err)
		}
		if id <= previous {
			t.Errorf("id of %d millis is %q, not after %q", millis, id, previous)
		}
		previous = id
	}
}

func TestNewRandomErrors(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected")
	tests := []struct {
		name   string
		random io.Reader
		want   error
	}{
		{"read fails", iotest.ErrReader(failure), failure},
		{"too few bytes", strings.NewReader("123456789"), io.ErrUnexpectedEOF},
		{"no bytes", strings.NewReader(""), io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, err := New(rfcTime(), tt.random)
			if id != "" || !errors.Is(err, tt.want) {
				t.Errorf("New = %q, %v, want an empty id and %v", id, err, tt.want)
			}
		})
	}
}

func TestValid(t *testing.T) {
	t.Parallel()
	const good = "017f22e2-79b0-7cc3-98c4-dc0c0c07398f"
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"canonical form", good, true},
		{"variant 8", "017f22e2-79b0-7cc3-88c4-dc0c0c07398f", true},
		{"variant a", "017f22e2-79b0-7cc3-a8c4-dc0c0c07398f", true},
		{"variant b", "017f22e2-79b0-7cc3-b8c4-dc0c0c07398f", true},
		{"empty", "", false},
		{"upper case", strings.ToUpper(good), false},
		{"one upper case digit", "017f22e2-79b0-7cc3-98c4-dc0c0c07398F", false},
		{"version 4", "017f22e2-79b0-4cc3-98c4-dc0c0c07398f", false},
		{"version 6", "017f22e2-79b0-6cc3-98c4-dc0c0c07398f", false},
		{"version 8", "017f22e2-79b0-8cc3-98c4-dc0c0c07398f", false},
		{"variant 0", "017f22e2-79b0-7cc3-08c4-dc0c0c07398f", false},
		{"variant 7", "017f22e2-79b0-7cc3-78c4-dc0c0c07398f", false},
		{"variant c", "017f22e2-79b0-7cc3-c8c4-dc0c0c07398f", false},
		{"variant f", "017f22e2-79b0-7cc3-f8c4-dc0c0c07398f", false},
		{"too short", good[:35], false},
		{"too long", good + "0", false},
		{"no hyphens", strings.ReplaceAll(good, "-", ""), false},
		{"digits in place of hyphens", strings.ReplaceAll(good, "-", "0"), false},
		{"hyphens in the wrong places", "017f22e279-b0-7cc3-98c4-dc0c0c07398f", false},
		{"first hyphen missing", "017f22e2_79b0-7cc3-98c4-dc0c0c07398f", false},
		{"last hyphen missing", "017f22e2-79b0-7cc3-98c4+dc0c0c07398f", false},
		{"not hexadecimal", "017f22e2-79b0-7cc3-98c4-dc0c0c07398g", false},
		{"braces", "{017f22e2-79b0-7cc3-98c4-dc0c0c07398f}", false},
		{"trailing newline", good + "\n", false},
		{"old 16 character id", "0123456789abcdef", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Valid(tt.id); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
