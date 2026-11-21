package cluster

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/SpliceFS/server/internal/uuid7"
)

// The random bytes of the tests: a seed of 32 bytes for the key, then the bytes that make the
// id, which is why the ids of the tests are those of testIDBytes at testNow.
var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // a constant of the tests

const testIDBytes = "ghijklmnop"

// testSeed is the seed of the key the tests make: 32 bytes, all different.
func testSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return seed
}

func testRandom() *bytes.Reader { return bytes.NewReader(append(testSeed(), testIDBytes...)) }

// testKey makes the key that the tests describe.
func testKey(t *testing.T) Key {
	t.Helper()
	made, err := NewKey(testNow, testRandom())
	if err != nil {
		t.Fatal(err)
	}
	return made
}

func testID(t *testing.T) string {
	t.Helper()
	id, err := uuid7.New(testNow, strings.NewReader(testIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// publicOf returns the public key of the Ed25519 key of seed: the half of the key after the seed.
func publicOf(seed []byte) []byte { return ed25519.NewKeyFromSeed(seed)[ed25519.SeedSize:] }

func TestNewKeyMakesTheKeyFromTheRandomBytes(t *testing.T) {
	t.Parallel()
	made := testKey(t)
	if made.ID != testID(t) {
		t.Errorf("ID = %q, want %q", made.ID, testID(t))
	}
	if want := ed25519.NewKeyFromSeed(testSeed()); !bytes.Equal(made.Private, want) {
		t.Error("Private is not the Ed25519 key of the first 32 random bytes")
	}
}

func TestNewKeyFailsWithoutRandomBytes(t *testing.T) {
	t.Parallel()
	seed := testSeed()
	tests := []struct {
		name   string
		random func() *bytes.Reader
	}{
		{"no seed", func() *bytes.Reader { return bytes.NewReader(seed[:ed25519.SeedSize-1]) }},
		{"no bytes for the id", func() *bytes.Reader { return bytes.NewReader(seed) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewKey(testNow, tt.random())
			if err == nil || !strings.Contains(err.Error(), "read random bytes") {
				t.Errorf("NewKey = %v, want an error about the random bytes", err)
			}
		})
	}
}

func TestNewKeyReportsARandomSourceThatFails(t *testing.T) {
	t.Parallel()
	errBroken := errors.New("entropy gone")
	if _, err := NewKey(testNow, iotest.ErrReader(errBroken)); !errors.Is(err, errBroken) {
		t.Errorf("NewKey = %v, want the error of the source", err)
	}
}

func TestPublicTextIsTheBase64OfThePublicKey(t *testing.T) {
	t.Parallel()
	want := b64(publicOf(testSeed()))
	if got := testKey(t).PublicText(); got != want {
		t.Errorf("PublicText = %q, want %q", got, want)
	}
}

// The layout of the file is part of the interface: an administrator reads it, a sync tool
// carries it, and every server of every version must read it.
func TestMarshalKeyIsTheFileLayout(t *testing.T) {
	t.Parallel()
	made := testKey(t)
	want := "id: " + testID(t) + "\n" +
		"private_key: " + b64(testSeed()) + "\n" +
		"public_key: " + made.PublicText() + "\n"
	if got := string(MarshalKey(made)); got != want {
		t.Errorf("MarshalKey =\n%s\nwant\n%s", got, want)
	}
}

func TestParseKeyReadsWhatMarshalKeyWrites(t *testing.T) {
	t.Parallel()
	made := testKey(t)
	got, err := ParseKey(MarshalKey(made))
	if err != nil || got.ID != made.ID || !bytes.Equal(got.Private, made.Private) {
		t.Errorf("ParseKey = %+v, %v, want %+v", got, err, made)
	}
}

// keyText is the content of a key file made of the given values.
func keyText(id, private, public string) string {
	return "id: " + id + "\nprivate_key: " + private + "\npublic_key: " + public + "\n"
}

func TestParseKeyRefuses(t *testing.T) {
	t.Parallel()
	made := testKey(t)
	private, public := b64(testSeed()), made.PublicText()
	other := b64(publicOf(bytes.Repeat([]byte{9}, ed25519.SeedSize)))
	tests := []struct {
		name string
		text string
		want error
	}{
		{"empty", "", errKeyFileForm},
		{"not yaml", "id: [\n", errKeyFileForm},
		{"a list", "- a\n- b\n", errKeyFileForm},
		{"a key that is not known", keyText(made.ID, private, public) + "extra: 1\n", errKeyFileForm},
		{"a key that is set twice", keyText(made.ID, private, public) + "id: " + made.ID + "\n", errKeyFileForm},
		{"no id", keyText("", private, public), errKeyID},
		{"an id that is not a UUIDv7", keyText("not-an-id", private, public), errKeyID},
		{"capital letters in the id", keyText(strings.ToUpper(made.ID), private, public), errKeyID},
		{"no private key", keyText(made.ID, "", public), errKeyForm},
		{"a private key that is not base64", keyText(made.ID, "!!!!", public), errKeyForm},
		{"a private key of the wrong size", keyText(made.ID, b64(testSeed()[:31]), public), errKeyForm},
		{"no public key", keyText(made.ID, private, ""), errKeyForm},
		{"a public key of the wrong size", keyText(made.ID, private, b64([]byte("short"))), errKeyForm},
		{"a public key of another private key", keyText(made.ID, private, other), errKeyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseKey([]byte(tt.text))
			if !errors.Is(err, tt.want) {
				t.Fatalf("ParseKey = %v, want %v", err, tt.want)
			}
			// The text of an error reaches the log, and the file holds a secret.
			if strings.Contains(err.Error(), private) {
				t.Errorf("the error holds the private key: %v", err)
			}
		})
	}
}

// A key with more than one spelling would look like two keys to a client that compares them as
// text, so the one spelling that MarshalKey writes is the only one read. Here the last
// character of the public key carries two bits that no byte uses; a decoder that does not check
// them reads the key all the same.
func TestParseKeyRefusesASpellingOfAKeyThatMarshalKeyDoesNotWrite(t *testing.T) {
	t.Parallel()
	made := testKey(t)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	text := made.PublicText()
	last := len(text) - len("=") - 1
	flipped := text[:last] + string(alphabet[strings.IndexByte(alphabet, text[last])^1]) + text[last+1:]
	if _, err := base64.StdEncoding.DecodeString(flipped); err != nil {
		t.Fatalf("the other spelling is not accepted by the decoder, so it proves nothing: %v", err)
	}
	if _, err := ParseKey([]byte(keyText(made.ID, b64(testSeed()), flipped))); !errors.Is(err, errKeyForm) {
		t.Errorf("ParseKey = %v, want %v", err, errKeyForm)
	}
}

func TestKeyEqual(t *testing.T) {
	t.Parallel()
	first, err := NewKey(testNow, bytes.NewReader(append(testSeed(), testIDBytes...)))
	if err != nil {
		t.Fatal(err)
	}
	other := bytes.NewReader(append(bytes.Repeat([]byte{9}, ed25519.SeedSize), testIDBytes...))
	second, err := NewKey(testNow, other)
	if err != nil {
		t.Fatal(err)
	}
	sameKeyOtherID := Key{ID: "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0d", Private: first.Private}
	tests := []struct {
		name  string
		other Key
		want  bool
	}{
		{"the same key", Key{ID: first.ID, Private: bytes.Clone(first.Private)}, true},
		{"another key", second, false},
		{"the same key under another id", sameKeyOtherID, false},
		{"no key", Key{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := first.Equal(tt.other); got != tt.want {
				t.Errorf("Equal = %v, want %v", got, tt.want)
			}
		})
	}
}
