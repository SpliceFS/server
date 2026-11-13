package auth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsersLookup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "alice.yaml", "password_hash: \"$argon2id$x\"\nshares: [media, docs]\n")
	writeFile(t, dir, "bob.yaml", "password_hash: h\nshares: [\"*\"]\ndisabled: true\n")
	users := NewUsers(dir)

	alice, err := users.Lookup("alice")
	if err != nil {
		t.Fatal(err)
	}
	want := User{Name: "alice", PasswordHash: "$argon2id$x", Shares: []string{"media", "docs"}}
	if !reflect.DeepEqual(alice, want) {
		t.Errorf("alice = %+v, want %+v", alice, want)
	}
	bob, err := users.Lookup("bob")
	if err != nil {
		t.Fatal(err)
	}
	want = User{Name: "bob", PasswordHash: "h", Shares: []string{"*"}, Disabled: true}
	if !reflect.DeepEqual(bob, want) {
		t.Errorf("bob = %+v, want %+v", bob, want)
	}
}

func TestUsersLookupSeesChangesAtOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	users := NewUsers(dir)
	if _, err := users.Lookup("carol"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("before add: err = %v, want ErrNoUser", err)
	}
	writeFile(t, dir, "carol.yaml", "password_hash: h1\n")
	user, err := users.Lookup("carol")
	if err != nil || user.PasswordHash != "h1" {
		t.Fatalf("after add: %+v, %v", user, err)
	}
	writeFile(t, dir, "carol.yaml", "password_hash: h2\n")
	user, err = users.Lookup("carol")
	if err != nil || user.PasswordHash != "h2" {
		t.Errorf("after change: hash = %q, err = %v, want h2", user.PasswordHash, err)
	}
	if err := os.Remove(filepath.Join(dir, "carol.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Lookup("carol"); !errors.Is(err, ErrNoUser) {
		t.Errorf("after remove: err = %v, want ErrNoUser", err)
	}
}

func TestUsersLookupNoUser(t *testing.T) {
	t.Parallel()
	// A decoy next to the users directory proves that bad names never reach the filesystem.
	parent := t.TempDir()
	dir := filepath.Join(parent, "users")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, parent, "outside.yaml", "password_hash: h\n")
	writeFile(t, dir, ".hidden.yaml", "password_hash: h\n")
	writeFile(t, dir, "Upper.yaml", "password_hash: h\n")
	users := NewUsers(dir)
	tests := []struct {
		name string
		user string
	}{
		{"missing", "nobody"},
		{"empty", ""},
		{"parent traversal", "../outside"},
		{"absolute path", filepath.Join(parent, "outside")},
		{"slash", "a/b"},
		{"backslash", `a\b`},
		{"dot only", "."},
		{"two dots", ".."},
		{"leading dot", ".hidden"},
		{"leading dash", "-x"},
		{"leading underscore", "_x"},
		{"uppercase", "Upper"},
		{"space", "a b"},
		{"nul byte", "a\x00b"},
		{"non ASCII", "caf\u00e9"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := users.Lookup(tt.user); !errors.Is(err, ErrNoUser) {
				t.Errorf("err = %v, want ErrNoUser", err)
			}
		})
	}
}

func TestUsersLookupNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	names := []string{"a", "0", "a.b", "a_b", "a-b", "user.name-1_x", strings.Repeat("a", 64)}
	for _, name := range names {
		writeFile(t, dir, name+".yaml", "password_hash: h\n")
	}
	users := NewUsers(dir)
	for _, name := range names {
		if u, err := users.Lookup(name); err != nil || u.Name != name {
			t.Errorf("Lookup(%q) = %+v, %v", name, u, err)
		}
	}
}

func TestUsersLookupBrokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "unreadable.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"not yaml", "password_hash: [\n"},
		{"wrong type", "password_hash: h\ndisabled: sesame-open\n"},
		{"unknown key", "password_hash: h\nadmin: true\n"},
		{"no hash", "shares: [media]\n"},
		{"empty hash", "password_hash: \"\"\n"},
	}
	for _, tt := range tests {
		writeFile(t, dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml", tt.content)
	}
	users := NewUsers(dir)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := users.Lookup(strings.ReplaceAll(tt.name, " ", "-"))
			if err == nil || errors.Is(err, ErrNoUser) {
				t.Errorf("err = %v, want an error other than ErrNoUser", err)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		if _, err := users.Lookup("unreadable"); err == nil || errors.Is(err, ErrNoUser) {
			t.Errorf("err = %v, want an error other than ErrNoUser", err)
		}
	})
}

// testHash stands in for a password hash that an administrator put in the wrong place.
const testHash = "$argon2id$v=19$m=19456,t=2,p=1$QlJTYWx0U2FsdDEyMw$Zk3pQ9xV7mWc1LhT5yRn8uJdA4sEoGbXiKfN2tYvM0w"

// leakWindow is the length of the piece of a secret that must not appear in an error.
const leakWindow = 6

// requireNoLeak fails when the text of err has any leakWindow characters of secret in it.
func requireNoLeak(t *testing.T, err error, secret string) {
	t.Helper()
	text := err.Error()
	for i := 0; i+leakWindow <= len(secret); i++ {
		if strings.Contains(text, secret[i:i+leakWindow]) {
			t.Fatalf("error %q contains %q of the secret", text, secret[i:i+leakWindow])
		}
	}
}

func TestUsersLookupBrokenFileDoesNotQuoteContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    error
		text    string
	}{
		{"hash under a key of another type", "shares: " + testHash + "\n", errUserWrongType, "line 1: "},
		{"hash where a boolean belongs", "password_hash: h\ndisabled: " + testHash + "\n", errUserWrongType, "line 2: "},
		{"hash under an unknown key", "password: " + testHash + "\n", errUserUnknownKey, "line 1: "},
		{"hash as an unknown key", testHash + ": x\n", errUserUnknownKey, "line 1: "},
		{"hash as a repeated key", testHash + ": x\n" + testHash + ": y\n", errUserDuplicateKey, "line 2: "},
		{"hash in an unclosed list", "password_hash: [" + testHash + "\n", errUserNotYAML, "line "},
		{"hash in an unclosed quote", "password_hash: '" + testHash + "\n", errUserNotYAML, "line "},
		{"hash as an unknown alias", "password_hash: *" + testHash[len(testHash)-43:] + "\n", errUserNotYAML, ""},
		{"two problems", "shares: " + testHash + "\ndisabled: " + testHash + "\n", errUserWrongType, "(and 1 more)"},
		{"empty file", "", errUserFileEmpty, ""},
	}
	dir := t.TempDir()
	for _, tt := range tests {
		writeFile(t, dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml", tt.content)
	}
	users := NewUsers(dir)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkBrokenUserFile(t, users, strings.ReplaceAll(tt.name, " ", "-"), tt.want, tt.text)
		})
	}
}

// checkBrokenUserFile looks up a user whose file is broken and checks the error: it is not
// ErrNoUser, no user is returned, no part of testHash is in it, it is of the kind want, and it
// names the user and contains text.
func checkBrokenUserFile(t *testing.T, users *Users, name string, want error, text string) {
	t.Helper()
	user, err := users.Lookup(name)
	if err == nil || errors.Is(err, ErrNoUser) {
		t.Fatalf("err = %v, want an error other than ErrNoUser", err)
	}
	if !reflect.DeepEqual(user, User{}) {
		t.Errorf("user = %+v, want no user, so that nobody can log in", user)
	}
	requireNoLeak(t, err, testHash)
	if !errors.Is(err, want) {
		t.Errorf("err = %q, want %q", err, want)
	}
	if !strings.Contains(err.Error(), "parse user "+strconv.Quote(name)) || !strings.Contains(err.Error(), text) {
		t.Errorf("err = %q, want it to name the user %q and contain %q", err, name, text)
	}
}

func TestUserProblem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{"wrong type", "line 4: cannot unmarshal !!str `x` into bool", "line 4: a value of the wrong type for its key"},
		{"unknown key", "line 2: field x not found in type auth.userFile", "line 2: a key that is not known"},
		{"repeated key", `line 3: mapping key "x" already defined at line 1`, "line 3: a key that is set twice"},
		{"other", "line 5: field x already set in type auth.userFile", "line 5: a key or value that cannot be used"},
		{"no line", "cannot unmarshal !!str `x` into bool", "a value of the wrong type for its key"},
		{"line is not a number", "line x: cannot unmarshal !!str `x` into bool", "a key or value that cannot be used"},
		{"line is zero", "line 0: cannot unmarshal !!str `x` into bool", "a key or value that cannot be used"},
		{"no colon after line", "line 3", "a key or value that cannot be used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := userProblem(tt.message).Error(); got != tt.want {
				t.Errorf("userProblem(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

func TestDescribeTypeErrorsWithoutMessages(t *testing.T) {
	t.Parallel()
	if got := describeTypeErrors(nil); !errors.Is(got, errUserOtherProblem) {
		t.Errorf("describeTypeErrors(nil) = %v, want %v", got, errUserOtherProblem)
	}
}

func TestUserAllows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		shares []string
		share  string
		want   bool
	}{
		{"listed", []string{"media", "docs"}, "docs", true},
		{"not listed", []string{"media"}, "docs", false},
		{"wildcard", []string{"*"}, "anything", true},
		{"wildcard among others", []string{"media", "*"}, "docs", true},
		{"none", nil, "media", false},
		{"empty share is not a wildcard", []string{"media"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (User{Shares: tt.shares}).Allows(tt.share); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.share, got, tt.want)
			}
		})
	}
}
