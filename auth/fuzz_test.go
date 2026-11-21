package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/config"
)

// fuzzTokens returns Tokens for the node under test, with one other node listed whose private
// key no seed holds, so that no token but the issued one verifies, and a clock that never
// moves, so every result depends on the input alone.
func fuzzTokens() *Tokens {
	stranger := wire.Node{Name: "other", URL: "https://other.example.com", ID: otherNode, SignKey: publicText(keyOf(3))}
	return newTestTokens(stubNodes{nodes: []wire.Node{stranger}}, newFakeClock())
}

func FuzzTokensVerify(f *testing.F) {
	tokens := fuzzTokens()
	issued, expires, err := tokens.Issue(aliceID)
	if err != nil {
		f.Fatal(err)
	}
	want := TokenClaims{UserID: aliceID, Expires: expires}
	header := tokenEncoding().EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + testNode + `"}`))
	otherHeader := tokenEncoding().EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + otherNode + `"}`))
	payload := tokenEncoding().EncodeToString(
		[]byte(`{"sub":"alice","shares":["*"],"iat":1,"exp":9999999999}`))
	for _, seed := range []string{
		issued, issued[:len(issued)-1], issued + "A", issued + ".", strings.ToUpper(issued),
		"", ".", "..", "...", "a.b.c", "Bearer " + issued,
		forge(keyOf(2), header, payload), forge(keyOf(1), "e30", payload), forge(keyOf(2), otherHeader, payload),
		forgeHMAC(publicText(keyOf(1)), tokenEncoding().EncodeToString(
			[]byte(`{"alg":"HS256","typ":"JWT","kid":"`+testNode+`"}`)), payload),
		header + "." + payload + ".", header + "." + payload + "." + strings.Repeat("A", 86),
		forge(keyOf(1), tokenEncoding().EncodeToString([]byte(`{"alg":"none","kid":"`+testNode+`"}`)), payload),
		strings.Repeat("a", 1<<16),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		claims, err := tokens.Verify(token)
		if err != nil {
			// Callers branch on these two errors, so nothing else may escape for input.
			if !errors.Is(err, api.ErrInvalidToken) && !errors.Is(err, api.ErrExpiredToken) {
				t.Fatalf("Verify(%q) failed with %v", token, err)
			}
			if claims != (TokenClaims{}) {
				t.Fatalf("Verify(%q) failed but returned %+v", token, claims)
			}
			return
		}
		// A token that was not issued never verifies: the only claims the server ever
		// signed are those of the one issued token, so any other result is a forgery.
		if claims != want {
			t.Fatalf("Verify(%q) = %+v, issued only %+v", token, claims, want)
		}
	})
}

func FuzzTokensVerifySigned(f *testing.F) {
	tokens := fuzzTokens()
	nowSec := newFakeClock().Now().Unix()
	for _, seed := range []string{
		fmt.Sprintf(`{"sub":"alice","shares":["media"],"iat":%d,"exp":%d}`, nowSec, nowSec+1800),
		fmt.Sprintf(`{"sub":"alice","iat":%d,"exp":%d}`, nowSec, nowSec+1800),
		fmt.Sprintf(`{"sub":"","iat":%d,"exp":%d}`, nowSec, nowSec+1800),
		fmt.Sprintf(`{"sub":"a","iat":%d}`, nowSec), `{"sub":"a","exp":1}`, `{}`, `null`, `[]`,
		`{"sub":"a","iat":9223372036854775807,"exp":9223372036854775807}`,
		`{"sub":"a","iat":-9223372036854775808,"exp":-9223372036854775808}`,
		`{"sub":"a","iat":1.5,"exp":2}`, `{"sub":1,"iat":1,"exp":2}`, `{"sub":"a","shares":"*"}`,
		``, `{`,
	} {
		f.Add([]byte(seed))
	}
	header := tokenEncoding().EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + testNode + `"}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		// The signature is genuine here, so this exercises the claims checks alone.
		token := forge(keyOf(1), header, tokenEncoding().EncodeToString(payload))
		claims, err := tokens.Verify(token)
		if err != nil {
			if !errors.Is(err, api.ErrInvalidToken) && !errors.Is(err, api.ErrExpiredToken) {
				t.Fatalf("Verify of payload %q failed with %v", payload, err)
			}
			return
		}
		// A token without a subject would be a login by nobody, so it must not verify.
		if claims.UserID == "" {
			t.Fatalf("payload %q verified with an empty user", payload)
		}
		// An accepted token is not expired beyond the leeway, so no stale token lives on.
		if newFakeClock().Now().Sub(claims.Expires) > Leeway {
			t.Fatalf("payload %q verified with expiry %v", payload, claims.Expires)
		}
	})
}

func FuzzParseHash(f *testing.F) {
	for _, seed := range []string{
		smallHash("pw"), smallHash(""),
		"$argon2id$v=19$m=65536,t=3,p=4$MDEyMzQ1Njc4OWFiY2RlZg$" +
			phcEncoding().EncodeToString(make([]byte, 32)),
		"", "$", "$$$$$", "$argon2id$v=19$m=8,t=1,p=1$$", "$argon2i$v=19$m=8,t=1,p=1$YQ$YQ",
		"$argon2id$v=18$m=8,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=0,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$m=1048577,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=8,t=11,p=1$YQ$YQ",
		"$argon2id$v=19$m=8,t=1,p=65$YQ$YQ", "$argon2id$v=19$m=8,t=1,p=1$YQ==$YQ",
		"$argon2id$v=19$m=-1,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=4294967296,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$t=1,m=8,p=1$YQ$YQ", "$argon2id$v=19$m=8,t=1$YQ$YQ",
		"x$argon2id$v=19$m=8,t=1,p=1$YQ$YQ",
		"$argon2id$v=19$m=8,t=1,p=1$YQ$YQ$",
	} {
		f.Add(seed)
	}
	f.Fuzz(checkParseHash)
}

// checkParseHash is the property of parseHash for one input: a refusal is safe and final,
// and an acceptance is within the limits.
func checkParseHash(t *testing.T, encoded string) {
	t.Helper()
	params, err := parseHash(encoded)
	if err != nil {
		checkRefusedHash(t, encoded, err)
		return
	}
	// A hostile user file must not make the server allocate or compute without limit.
	if !withinHashLimits(params) {
		t.Fatalf("parseHash(%q) accepted out of range %+v", encoded, params)
	}
	// Argon2 is slow by design, so only cheap parameter sets are verified here.
	if params.memory <= 64 && params.time <= 2 && params.threads <= 4 && len(params.key) <= 64 {
		if _, err := VerifyPassword(encoded, "pw"); err != nil {
			t.Fatalf("VerifyPassword(%q) = %v after parseHash succeeded", encoded, err)
		}
	}
}

// withinHashLimits states the limits of VerifyPassword again, apart from the code under
// test, so that a mistake in that code cannot hide itself.
func withinHashLimits(params hashParams) bool {
	return params.memory >= 1 && params.memory <= maxMemory &&
		params.time >= 1 && params.time <= maxTime &&
		params.threads >= 1 && params.threads <= maxThreads &&
		len(params.salt) > 0 && len(params.key) > 0
}

// checkRefusedHash checks the error of a refused hash: it does not repeat the hash, and
// VerifyPassword refuses the hash as well.
func checkRefusedHash(t *testing.T, encoded string, refusal error) {
	t.Helper()
	// The string is a stored secret, so an error must not repeat it into logs.
	if len(encoded) >= 48 && strings.Contains(refusal.Error(), encoded) {
		t.Fatalf("error for %q repeats the hash", encoded)
	}
	if _, err := VerifyPassword(encoded, "pw"); err == nil {
		t.Fatalf("VerifyPassword accepted %q that parseHash refused", encoded)
	}
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
			phcEncoding().EncodeToString(salt), phcEncoding().EncodeToString(key))
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
	users := newTestUsers(filepath.Join(dir, "users"))
	if err := os.Mkdir(filepath.Join(dir, "users"), 0o700); err != nil {
		f.Fatal(err)
	}
	// A file next to the users directory is what a name with ".." would reach.
	writeFile(f, dir, "outside.yaml", "name: outside\npassword_hash: x\n")
	writeUser(f, filepath.Join(dir, "users"), aliceID, "name: alice\npassword_hash: x\n")
	for _, seed := range []string{
		"alice", "bob", "a", "", ".", "..", "../outside", "..%2foutside", "a/b", `a\b`, "/etc/passwd",
		"Alice", "-a", ".a", "a.b", "a_b-c", "a\x00b", "alice\x00", strings.Repeat("a", 64),
		strings.Repeat("a", 65), "al ice", "outside", "../users/alice",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		checkUserName(t, users, name)
	})
}

// checkUserName is the property of Lookup for one name: only the user whose file exists is found,
// and a name that is not a valid user name finds nothing.
func checkUserName(t *testing.T, users *Users, name string) {
	t.Helper()
	user, found := users.Lookup(name)
	if !config.ValidName(name) {
		// An invalid name is refused before any file is touched.
		if found {
			t.Fatalf("Lookup(%q) found %+v", name, user)
		}
		return
	}
	// A valid name has no separator or dot segment, so it names no file at all.
	if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		t.Fatalf("config.ValidName(%q) is true", name)
	}
	// Only the user whose file exists may be found, so no name reaches another file.
	if found != (name == "alice") || (found && user.Name != name) {
		t.Fatalf("Lookup(%q) = %+v, %v", name, user, found)
	}
}

func FuzzUserFile(f *testing.F) {
	for _, seed := range []string{
		"name: a\npassword_hash: x\n", "name: a\npassword_hash: x\nshares: [a, '*']\ndisabled: true\n",
		"name: a\npassword_hash: ''\n", "", "---\n---\n", "shares: [a]\n", "name: a\npassword_hash: x\nextra: 1\n",
		"name: a\npassword_hash: [x]\n", "name: a\npassword_hash: x\nshares: a\n", "password_hash: x\n",
		"name: a\npassword_hash: x\ndisabled: maybe\n", "name: ../a\npassword_hash: x\n",
		"- a\n", "{", "&a [*a]", "a: &a [*a, *a]\n", "\x00", "name: a\npassword_hash: \"\\ud800\"\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		user, err := parseUser(aliceID+".yaml", content)
		if err != nil {
			if !reflect.DeepEqual(user, User{}) {
				t.Fatalf("file %q gave %+v with an error", content, user)
			}
			return
		}
		// A user without a password hash would be a user anyone could be, or no one; a name
		// that is not valid would not be found at login.
		if user.PasswordHash == "" || !config.ValidName(user.Name) || user.ID != aliceID {
			t.Fatalf("file %q gave %+v", content, user)
		}
	})
}
