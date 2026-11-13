package auth

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// smallHash builds a valid PHC string with cheap parameters.
func smallHash(password string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 8, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1$%s$%s",
		phcEncoding().EncodeToString(salt), phcEncoding().EncodeToString(key))
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

func TestHashPasswordFormat(t *testing.T) {
	t.Parallel()
	salt := []byte("0123456789abcdef")
	hash, err := HashPassword("correct horse", bytes.NewReader(salt))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(hash, "$")
	if len(fields) != 6 || fields[1] != "argon2id" || fields[2] != "v=19" || fields[3] != "m=65536,t=3,p=4" {
		t.Fatalf("unexpected format: %q", hash)
	}
	if got := phcEncoding().EncodeToString(salt); fields[4] != got {
		t.Errorf("salt field = %q, want %q", fields[4], got)
	}
	if !isUnpaddedKey(fields[5]) {
		t.Errorf("hash field %q is not 32 bytes of unpadded base64", fields[5])
	}
}

// isUnpaddedKey reports whether field is the unpadded base64 of a 32 byte key.
func isUnpaddedKey(field string) bool {
	key, err := phcEncoding().DecodeString(field)
	return err == nil && len(key) == 32 && !strings.Contains(field, "=")
}

func TestHashPasswordRoundTrip(t *testing.T) {
	t.Parallel()
	hash, err := HashPassword("correct horse", bytes.NewReader([]byte("0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(hash, "correct horse")
	if err != nil || !ok {
		t.Fatalf("round trip = %v, %v", ok, err)
	}
}

func TestHashPasswordErrors(t *testing.T) {
	t.Parallel()
	if _, err := HashPassword("", bytes.NewReader(make([]byte, 16))); !errors.Is(err, errEmptyPassword) {
		t.Errorf("empty password: err = %v, want errEmptyPassword", err)
	}
	_, err := HashPassword("secret-password", failingReader{})
	if err == nil {
		t.Fatal("failing reader: want error")
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Errorf("error leaks the password: %v", err)
	}
	if _, err := HashPassword("secret-password", bytes.NewReader(make([]byte, 5))); err == nil {
		t.Error("short random source: want error")
	}
}

func TestVerifyPassword(t *testing.T) {
	t.Parallel()
	hash := smallHash("hunter2")
	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"match", "hunter2", true},
		{"wrong", "hunter3", false},
		{"empty", "", false},
		{"prefix", "hunter", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := VerifyPassword(hash, tt.password)
			if err != nil || got != tt.want {
				t.Errorf("VerifyPassword = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}

func TestVerifyPasswordUsesStoredParameters(t *testing.T) {
	t.Parallel()
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("pw"), salt, 2, 16, 2, 20)
	hash := fmt.Sprintf("$argon2id$v=19$m=16,t=2,p=2$%s$%s",
		phcEncoding().EncodeToString(salt), phcEncoding().EncodeToString(key))
	ok, err := VerifyPassword(hash, "pw")
	if err != nil || !ok {
		t.Errorf("VerifyPassword = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	t.Parallel()
	valid := smallHash("pw")
	salt := phcEncoding().EncodeToString([]byte("0123456789abcdef"))
	key := phcEncoding().EncodeToString(make([]byte, 32))
	build := func(algo, version, costs, salt, key string) string {
		return strings.Join([]string{"", algo, version, costs, salt, key}, "$")
	}
	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"plain text", "hunter2"},
		{"missing field", "$argon2id$v=19$m=8,t=1,p=1$" + salt},
		{"extra field", valid + "$x"},
		{"no leading dollar", "x" + valid},
		{"wrong algorithm", build("argon2i", "v=19", "m=8,t=1,p=1", salt, key)},
		{"bcrypt", "$2y$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345"},
		{"old version", build("argon2id", "v=16", "m=8,t=1,p=1", salt, key)},
		{"bad version", build("argon2id", "version", "m=8,t=1,p=1", salt, key)},
		{"missing params", build("argon2id", "v=19", "m=8,t=1", salt, key)},
		{"extra params", build("argon2id", "v=19", "m=8,t=1,p=1,x=1", salt, key)},
		{"wrong param order", build("argon2id", "v=19", "t=1,m=8,p=1", salt, key)},
		{"non-numeric param", build("argon2id", "v=19", "m=eight,t=1,p=1", salt, key)},
		{"negative param", build("argon2id", "v=19", "m=8,t=-1,p=1", salt, key)},
		{"param overflow", build("argon2id", "v=19", "m=99999999999,t=1,p=1", salt, key)},
		{"zero memory", build("argon2id", "v=19", "m=0,t=1,p=1", salt, key)},
		{"zero time", build("argon2id", "v=19", "m=8,t=0,p=1", salt, key)},
		{"zero threads", build("argon2id", "v=19", "m=8,t=1,p=0", salt, key)},
		{"memory above 1 GiB", build("argon2id", "v=19", "m=1048577,t=1,p=1", salt, key)},
		{"time above 10", build("argon2id", "v=19", "m=8,t=11,p=1", salt, key)},
		{"threads above 64", build("argon2id", "v=19", "m=8,t=1,p=65", salt, key)},
		{"threads overflow uint8", build("argon2id", "v=19", "m=8,t=1,p=260", salt, key)},
		{"bad salt base64", build("argon2id", "v=19", "m=8,t=1,p=1", "!!!", key)},
		{"padded salt", build("argon2id", "v=19", "m=8,t=1,p=1", salt+"==", key)},
		{"empty salt", build("argon2id", "v=19", "m=8,t=1,p=1", "", key)},
		{"bad hash base64", build("argon2id", "v=19", "m=8,t=1,p=1", salt, "!!!")},
		{"empty hash", build("argon2id", "v=19", "m=8,t=1,p=1", salt, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok, err := VerifyPassword(tt.encoded, "pw")
			if err == nil || ok {
				t.Fatalf("VerifyPassword = %v, %v; want false and an error", ok, err)
			}
			if tt.encoded != "" && strings.Contains(err.Error(), tt.encoded) {
				t.Errorf("error contains the hash: %v", err)
			}
		})
	}
}

func TestVerifyPasswordAcceptsLimits(t *testing.T) {
	t.Parallel()
	// The upper limits are valid in the parser; only the cheap ones are computed here.
	costs, err := parseCosts("m=1048576,t=10,p=64")
	if err != nil {
		t.Fatal(err)
	}
	if costs.memory != maxMemory || costs.time != maxTime || costs.threads != maxThreads {
		t.Errorf("costs = %+v", costs)
	}
}
