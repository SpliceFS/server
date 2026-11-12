package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// fuzzTokens writes a keys file with one key and returns Tokens over it, with a clock
// that never moves, so every result depends on the input alone.
func fuzzTokens(f *testing.F) *Tokens {
	f.Helper()
	path := filepath.Join(f.TempDir(), "keys.yaml")
	content := keysYAML([2]string{"new", b64(secretA)})
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.Fatal(err)
	}
	return NewTokens(path, 30*time.Minute, newFakeClock().Now)
}

func FuzzTokensVerify(f *testing.F) {
	tokens := fuzzTokens(f)
	issued, expires, err := tokens.Issue("alice", []string{"media"})
	if err != nil {
		f.Fatal(err)
	}
	want := Claims{User: "alice", Shares: []string{"media"}, Expires: expires}
	header := tokenEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT","kid":"new"}`))
	payload := tokenEncoding.EncodeToString(
		[]byte(`{"sub":"alice","shares":["*"],"iat":1,"exp":9999999999}`))
	for _, s := range []string{
		issued, issued[:len(issued)-1], issued + "A", issued + ".", strings.ToUpper(issued),
		"", ".", "..", "...", "a.b.c", "Bearer " + issued,
		forge(secretB, header, payload), forge(secretA, "e30", payload),
		header + "." + payload + ".", header + "." + payload + "." + strings.Repeat("A", 43),
		forge(secretA, tokenEncoding.EncodeToString([]byte(`{"alg":"none","kid":"new"}`)), payload),
		strings.Repeat("a", 1<<16),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		claims, err := tokens.Verify(token)
		if err != nil {
			// Callers branch on these two errors, so nothing else may escape for input.
			if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, ErrExpiredToken) {
				t.Fatalf("Verify(%q) failed with %v", token, err)
			}
			if claims.User != "" || claims.Shares != nil {
				t.Fatalf("Verify(%q) failed but returned %+v", token, claims)
			}
			return
		}
		// A token that was not issued never verifies: the only claims the server ever
		// signed are those of the one issued token, so any other result is a forgery.
		if !reflect.DeepEqual(claims, want) {
			t.Fatalf("Verify(%q) = %+v, issued only %+v", token, claims, want)
		}
	})
}

func FuzzTokensVerifySigned(f *testing.F) {
	tokens := fuzzTokens(f)
	now := newFakeClock().Now().Unix()
	for _, s := range []string{
		fmt.Sprintf(`{"sub":"alice","shares":["media"],"iat":%d,"exp":%d}`, now, now+1800),
		fmt.Sprintf(`{"sub":"alice","iat":%d,"exp":%d}`, now, now+1800),
		fmt.Sprintf(`{"sub":"","iat":%d,"exp":%d}`, now, now+1800),
		fmt.Sprintf(`{"sub":"a","iat":%d}`, now), `{"sub":"a","exp":1}`, `{}`, `null`, `[]`,
		`{"sub":"a","iat":9223372036854775807,"exp":9223372036854775807}`,
		`{"sub":"a","iat":-9223372036854775808,"exp":-9223372036854775808}`,
		`{"sub":"a","iat":1.5,"exp":2}`, `{"sub":1,"iat":1,"exp":2}`, `{"sub":"a","shares":"*"}`,
		``, `{`,
	} {
		f.Add([]byte(s))
	}
	header := tokenEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT","kid":"new"}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		// The signature is genuine here, so this exercises the claims checks alone.
		token := forge(secretA, header, tokenEncoding.EncodeToString(payload))
		claims, err := tokens.Verify(token)
		if err != nil {
			if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, ErrExpiredToken) {
				t.Fatalf("Verify of payload %q failed with %v", payload, err)
			}
			return
		}
		// A token without a subject would be a login by nobody, so it must not verify.
		if claims.User == "" {
			t.Fatalf("payload %q verified with an empty user", payload)
		}
		// An accepted token is not expired beyond the leeway, so no stale token lives on.
		if newFakeClock().Now().Sub(claims.Expires) > Leeway {
			t.Fatalf("payload %q verified with expiry %v", payload, claims.Expires)
		}
	})
}

func FuzzParseHash(f *testing.F) {
	for _, s := range []string{
		smallHash("pw"), smallHash(""),
		"$argon2id$v=19$m=65536,t=3,p=4$MDEyMzQ1Njc4OWFiY2RlZg$" +
			phcEncoding.EncodeToString(make([]byte, 32)),
		"", "$", "$$$$$", "$argon2id$v=19$m=8,t=1,p=1$$", "$argon2i$v=19$m=8,t=1,p=1$YQ$YQ",
		"$argon2id$v=18$m=8,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=0,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$m=1048577,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=8,t=11,p=1$YQ$YQ",
		"$argon2id$v=19$m=8,t=1,p=65$YQ$YQ", "$argon2id$v=19$m=8,t=1,p=1$YQ==$YQ",
		"$argon2id$v=19$m=-1,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=4294967296,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$t=1,m=8,p=1$YQ$YQ", "$argon2id$v=19$m=8,t=1$YQ$YQ",
		"x$argon2id$v=19$m=8,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$m=8,t=1,p=1$YQ$YQ$",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		h, err := parseHash(encoded)
		if err != nil {
			// The string is a stored secret, so an error must not repeat it into logs.
			if len(encoded) >= 48 && strings.Contains(err.Error(), encoded) {
				t.Fatalf("error for %q repeats the hash", encoded)
			}
			if _, verr := VerifyPassword(encoded, "pw"); verr == nil {
				t.Fatalf("VerifyPassword accepted %q that parseHash refused", encoded)
			}
			return
		}
		// A hostile user file must not make the server allocate or compute without limit.
		if h.memory < 1 || h.memory > maxMemory || h.time < 1 || h.time > maxTime ||
			h.threads < 1 || h.threads > maxThreads || len(h.salt) == 0 || len(h.key) == 0 {
			t.Fatalf("parseHash(%q) accepted out of range %+v", encoded, h)
		}
		// Argon2 is slow by design, so only cheap parameter sets are verified here.
		if h.memory <= 64 && h.time <= 2 && h.threads <= 4 && len(h.key) <= 64 {
			if _, err := VerifyPassword(encoded, "pw"); err != nil {
				t.Fatalf("VerifyPassword(%q) = %v after parseHash succeeded", encoded, err)
			}
		}
	})
}

func FuzzVerifyPasswordRoundTrip(f *testing.F) {
	f.Add("correct horse", []byte("0123456789abcdef"))
	f.Add("", []byte("s"))
	f.Add("\x00", []byte{0})
	f.Add(strings.Repeat("p", 1000), make([]byte, 64))
	f.Fuzz(func(t *testing.T, password string, salt []byte) {
		if len(salt) == 0 {
			t.Skip("an empty salt is refused by parseHash")
		}
		key := argon2.IDKey([]byte(password), salt, 1, 8, 1, 32)
		encoded := fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1$%s$%s",
			phcEncoding.EncodeToString(salt), phcEncoding.EncodeToString(key))
		// A hash written for a password must accept that password and no other.
		ok, err := VerifyPassword(encoded, password)
		if err != nil || !ok {
			t.Fatalf("VerifyPassword(own hash) = %v, %v", ok, err)
		}
		ok, err = VerifyPassword(encoded, password+"x")
		if err != nil || ok {
			t.Fatalf("VerifyPassword accepted a different password: %v, %v", ok, err)
		}
	})
}

func FuzzUserName(f *testing.F) {
	dir := f.TempDir()
	users := NewUsers(filepath.Join(dir, "users"))
	if err := os.Mkdir(filepath.Join(dir, "users"), 0o700); err != nil {
		f.Fatal(err)
	}
	hash := "password_hash: x\n"
	// A file next to the users directory is what a name with ".." would reach.
	if err := os.WriteFile(filepath.Join(dir, "outside.yaml"), []byte(hash), 0o600); err != nil {
		f.Fatal(err)
	}
	alice := filepath.Join(dir, "users", "alice.yaml")
	if err := os.WriteFile(alice, []byte(hash), 0o600); err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{
		"alice", "bob", "a", "", ".", "..", "../outside", "..%2foutside", "a/b", `a\b`, "/etc/passwd",
		"Alice", "-a", ".a", "a.b", "a_b-c", "a\x00b", "alice\x00", strings.Repeat("a", 64),
		strings.Repeat("a", 65), "al ice", "outside", "../users/alice",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		user, err := users.Lookup(name)
		if !validName(name) {
			// An invalid name is refused before any file is touched.
			if !errors.Is(err, ErrNoUser) {
				t.Fatalf("Lookup(%q) = %v", name, err)
			}
			return
		}
		// A valid name has no separator or dot segment, so its file stays in the directory.
		if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
			t.Fatalf("validName(%q) is true", name)
		}
		// Only the user whose file exists may be found, so no name reaches another file.
		if (err == nil) != (name == "alice") || (err != nil && !errors.Is(err, ErrNoUser)) {
			t.Fatalf("Lookup(%q) = %v", name, err)
		}
		if err == nil && user.Name != name {
			t.Fatalf("Lookup(%q) returned user %q", name, user.Name)
		}
	})
}

func FuzzUserFile(f *testing.F) {
	dir := f.TempDir()
	users := NewUsers(dir)
	for _, s := range []string{
		"password_hash: x\n", "password_hash: x\nshares: [a, '*']\ndisabled: true\n",
		"password_hash: ''\n", "", "---\n---\n", "shares: [a]\n", "password_hash: x\nextra: 1\n",
		"password_hash: [x]\n", "password_hash: x\nshares: a\n", "password_hash: x\ndisabled: maybe\n",
		"- a\n", "{", "&a [*a]", "a: &a [*a, *a]\n", "\x00", "password_hash: \"\\ud800\"\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		if err := os.WriteFile(filepath.Join(dir, "alice.yaml"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		user, err := users.Lookup("alice")
		if err != nil {
			// A broken file is an operator mistake and must not look like a missing user.
			if errors.Is(err, ErrNoUser) {
				t.Fatalf("Lookup of file %q = ErrNoUser", content)
			}
			return
		}
		// A user without a password hash would be a user anyone could be, or no one.
		if user.PasswordHash == "" || user.Name != "alice" {
			t.Fatalf("file %q gave %+v", content, user)
		}
	})
}

func FuzzReadKeys(f *testing.F) {
	path := filepath.Join(f.TempDir(), "keys.yaml")
	for _, s := range []string{
		twoKeys(), keysYAML([2]string{"a", b64(secretA)}), keysYAML([2]string{"", b64(secretA)}),
		keysYAML([2]string{"a", b64(secretA)}, [2]string{"a", b64(secretB)}),
		keysYAML([2]string{"a", "short"}), keysYAML([2]string{"a", "!!!"}),
		keysYAML([2]string{"a", b64(secretA[:31])}), "keys: []\n", "", "keys:\n  - id: a\n    x: 1\n",
		"keys: a\n", "{", "---\n---\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		keys, err := readKeys(path)
		if err != nil {
			if keys != nil {
				t.Fatalf("readKeys failed but returned %d keys", len(keys))
			}
			return
		}
		// A weak, unnamed or duplicated key would make tokens forgeable or ambiguous.
		seen := map[string]bool{}
		for _, k := range keys {
			if k.id == "" || seen[k.id] || len(k.secret) < minSecretSize {
				t.Fatalf("file %q accepted key %q with %d secret bytes", content, k.id, len(k.secret))
			}
			seen[k.id] = true
		}
		if len(keys) == 0 {
			t.Fatalf("file %q accepted with no keys", content)
		}
	})
}
