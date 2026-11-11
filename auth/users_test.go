package auth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	u, err := users.Lookup("carol")
	if err != nil || u.PasswordHash != "h1" {
		t.Fatalf("after add: %+v, %v", u, err)
	}
	writeFile(t, dir, "carol.yaml", "password_hash: h2\n")
	if u, _ := users.Lookup("carol"); u.PasswordHash != "h2" {
		t.Errorf("after change: hash = %q, want h2", u.PasswordHash)
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
		{"dot dot", ".."},
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
