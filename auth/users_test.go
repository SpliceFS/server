package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// User ids of the tests. A later id sorts after an earlier one, as the ids of users made later do.
const (
	aliceID = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0d"
	bobID   = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0e"
	carolID = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0f"
	daveID  = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c10"
)

func writeFile(tb testing.TB, dir, name, content string) {
	tb.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		tb.Fatal(err)
	}
}

// writeUser writes the file of the user with the given id.
func writeUser(tb testing.TB, dir, id, content string) {
	tb.Helper()
	writeFile(tb, dir, id+".yaml", content)
}

// logCapture returns a logger that writes its lines to the returned buffer, and the buffer.
func logCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// newTestUsers returns Users over dir that discard their log, on a clock that does not move.
func newTestUsers(dir string) *Users {
	return newUsersAt(dir, newFakeClock())
}

// newUsersAt returns Users over dir that discard their log and read the time from clock.
func newUsersAt(dir string, clock *fakeClock) *Users {
	return NewUsers(dir, clock.Now, slog.New(slog.DiscardHandler))
}

func TestUsersLookup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: \"$argon2id$x\"\nshares: [media, docs]\n")
	writeUser(t, dir, bobID, "name: bob\npassword_hash: h\nshares: [\"*\"]\ndisabled: true\n")
	users := newTestUsers(dir)

	alice, found := users.Lookup("alice")
	if !found {
		t.Fatal("alice not found")
	}
	want := User{ID: aliceID, Name: "alice", PasswordHash: "$argon2id$x", Shares: []string{"media", "docs"}}
	if !reflect.DeepEqual(alice, want) {
		t.Errorf("alice = %+v, want %+v", alice, want)
	}
	bob, found := users.Lookup("bob")
	if !found {
		t.Fatal("bob not found")
	}
	want = User{ID: bobID, Name: "bob", PasswordHash: "h", Shares: []string{"*"}, Disabled: true}
	if !reflect.DeepEqual(bob, want) {
		t.Errorf("bob = %+v, want %+v", bob, want)
	}
}

func TestUsersLookupSeesChangesAtOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	users := newTestUsers(dir)
	if _, found := users.Lookup("carol"); found {
		t.Fatal("before add: found the user")
	}
	writeUser(t, dir, carolID, "name: carol\npassword_hash: h1\n")
	user, found := users.Lookup("carol")
	if !found || user.PasswordHash != "h1" {
		t.Fatalf("after add: %+v, %v", user, found)
	}
	writeUser(t, dir, carolID, "name: carol\npassword_hash: h2\n")
	user, found = users.Lookup("carol")
	if !found || user.PasswordHash != "h2" {
		t.Errorf("after change: hash = %q, found = %v, want h2", user.PasswordHash, found)
	}
}

func TestUsersLookupSeesRenameAndRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	users := newTestUsers(dir)
	writeUser(t, dir, carolID, "name: carol\npassword_hash: h\n")
	if _, found := users.Lookup("carol"); !found {
		t.Fatal("carol not found")
	}
	writeUser(t, dir, carolID, "name: caroline\npassword_hash: h\n")
	if _, found := users.Lookup("carol"); found {
		t.Error("after rename: still found carol")
	}
	if user, found := users.Lookup("caroline"); !found || user.ID != carolID {
		t.Errorf("after rename: Lookup(caroline) = %+v, %v", user, found)
	}
	if err := os.Remove(filepath.Join(dir, carolID+".yaml")); err != nil {
		t.Fatal(err)
	}
	if _, found := users.Lookup("caroline"); found {
		t.Error("after remove: still found caroline")
	}
}

func TestUsersLookupNoUser(t *testing.T) {
	t.Parallel()
	// A decoy next to the users directory proves that a name never reaches the file system.
	parent := t.TempDir()
	dir := filepath.Join(parent, "users")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, parent, "outside.yaml", "name: outside\npassword_hash: h\n")
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: h\n")
	users := newTestUsers(dir)
	tests := []struct {
		name string
		user string
	}{
		{"missing", "nobody"},
		{"empty", ""},
		{"the id of a user", aliceID},
		{"parent traversal", "../outside"},
		{"absolute path", filepath.Join(parent, "outside")},
		{"slash", "a/b"},
		{"backslash", `a\b`},
		{"dot only", "."},
		{"two dots", ".."},
		{"leading dot", ".hidden"},
		{"leading dash", "-x"},
		{"leading underscore", "_x"},
		{"uppercase", "Alice"},
		{"space", "a b"},
		{"nul byte", "a\x00b"},
		{"non ASCII", "caf\u00e9"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, found := users.Lookup(tt.user); found {
				t.Error("found a user, want none")
			}
		})
	}
}

func TestUsersLookupNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	names := []string{"a", "0", "a.b", "a_b", "a-b", "user.name-1_x", strings.Repeat("a", 64)}
	for i, name := range names {
		writeUser(t, dir, fmt.Sprintf("0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c%02x", i), "name: "+name+"\npassword_hash: h\n")
	}
	users := newTestUsers(dir)
	for _, name := range names {
		if u, found := users.Lookup(name); !found || u.Name != name {
			t.Errorf("Lookup(%q) = %+v, %v", name, u, found)
		}
	}
}

func TestUsersLookupResultIsACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: h\nshares: [media]\n")
	users := newTestUsers(dir)
	first, found := users.Lookup("alice")
	if !found {
		t.Fatal("alice not found")
	}
	first.Shares[0] = "changed"
	if again, found := users.Lookup("alice"); !found || again.Shares[0] != "media" {
		t.Errorf("a second lookup = %+v, %v, want the shares unchanged", again, found)
	}
}

func TestUsersLookupDuplicateNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, logged := logCapture()
	writeUser(t, dir, bobID, "name: alice\npassword_hash: second\n")
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: first\n")
	users := NewUsers(dir, newFakeClock().Now, log)
	for range 2 {
		user, found := users.Lookup("alice")
		if !found || user.ID != aliceID || user.PasswordHash != "first" {
			t.Fatalf("Lookup(alice) = %+v, %v, want the file that sorts first", user, found)
		}
	}
	text := logged.String()
	for _, want := range []string{"duplicate user name", "kept=" + aliceID + ".yaml", "ignored=" + bobID + ".yaml"} {
		if !strings.Contains(text, want) {
			t.Errorf("log %q lacks %q", text, want)
		}
	}
	if got := strings.Count(text, "duplicate user name"); got != 1 {
		t.Errorf("the clash was logged %d times, want once for one change of the files", got)
	}
}

func TestUsersSkipsFilesNotNamedByID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, logged := logCapture()
	writeFile(t, dir, "alice.yaml", "name: alice\npassword_hash: h\n")
	writeUser(t, dir, bobID, "name: bob\npassword_hash: h\n")
	users := NewUsers(dir, newFakeClock().Now, log)
	if _, found := users.Lookup("alice"); found {
		t.Error("Lookup(alice) found a file named by the user name")
	}
	if _, found := users.Lookup("bob"); !found {
		t.Error("Lookup(bob) found nothing, want the user of the other file")
	}
	want := "users/alice.yaml: user files are named by the user id since this version; " +
		"run splicefsdctl user add to make one"
	if !strings.Contains(logged.String(), want) {
		t.Errorf("log %q lacks the warning %q", logged.String(), want)
	}
}

func TestUsersKeepLastGoodContentOfABrokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, logged := logCapture()
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: good\n")
	users := NewUsers(dir, newFakeClock().Now, log)
	if _, found := users.Lookup("alice"); !found {
		t.Fatal("alice not found")
	}
	writeUser(t, dir, aliceID, "name: alice\npassword_hash: [\n")
	user, found := users.Lookup("alice")
	if !found || user.PasswordHash != "good" {
		t.Errorf("Lookup(alice) = %+v, %v, want the last good content", user, found)
	}
	if !strings.Contains(logged.String(), "keeping its last good content") {
		t.Errorf("log %q has no warning", logged.String())
	}
}

func TestParseUserBrokenFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"empty file", "", errUserFileEmpty},
		{"not yaml", "name: a\npassword_hash: [\n", errUserNotYAML},
		{"wrong type", "name: a\npassword_hash: h\ndisabled: sesame-open\n", errUserWrongType},
		{"unknown key", "name: a\npassword_hash: h\nadmin: true\n", errUserUnknownKey},
		{"no name", "password_hash: h\n", errEmptyName},
		{"empty name", "name: \"\"\npassword_hash: h\n", errEmptyName},
		{"name with a capital", "name: Alice\npassword_hash: h\n", errBadName},
		{"name with a slash", "name: a/b\npassword_hash: h\n", errBadName},
		{"no hash", "name: a\nshares: [media]\n", errEmptyPasswordHash},
		{"empty hash", "name: a\npassword_hash: \"\"\n", errEmptyPasswordHash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			user, err := parseUser(aliceID+".yaml", []byte(tt.content))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if !reflect.DeepEqual(user, User{}) {
				t.Errorf("user = %+v, want no user, so that nobody can log in", user)
			}
		})
	}
}

func TestParseUserFileNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
		ok   bool
	}{
		{"user id", aliceID + ".yaml", true},
		{"user name", "alice.yaml", false},
		{"id in capitals", strings.ToUpper(aliceID) + ".yaml", false},
		{"id of another version", "0193f5c2-7d1e-4b2a-9c4d-5e6f7a8b9c0d.yaml", false},
		{"no extension", aliceID, false},
		{"other extension", aliceID + ".yml", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseUser(tt.file, []byte("name: a\npassword_hash: h\n"))
			if (err == nil) != tt.ok || (err != nil && !errors.Is(err, errNotUserFile)) {
				t.Errorf("parseUser(%q) error = %v, want ok = %v", tt.file, err, tt.ok)
			}
		})
	}
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

func TestParseUserBrokenFileDoesNotQuoteContent(t *testing.T) {
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
		{"hash as the name", "name: " + testHash + "\npassword_hash: h\n", errBadName, ""},
		{"empty file", "", errUserFileEmpty, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseUser(aliceID+".yaml", []byte(tt.content))
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			requireNoLeak(t, err, testHash)
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.text) {
				t.Errorf("err = %q, want %q containing %q", err, tt.want, tt.text)
			}
		})
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

// currentFixture is Users over a temporary folder, on a clock the test moves, whose check of a
// file is counted and can be made to fail.
type currentFixture struct {
	dir     string
	clock   *fakeClock
	users   *Users
	stats   int
	statErr error
	logged  *bytes.Buffer
}

func newCurrentFixture(t *testing.T) *currentFixture {
	t.Helper()
	fixture := &currentFixture{dir: t.TempDir(), clock: newFakeClock()}
	var log *slog.Logger
	log, fixture.logged = logCapture()
	fixture.users = NewUsers(fixture.dir, fixture.clock.Now, log)
	fixture.users.stat = func(path string) (fs.FileInfo, error) {
		fixture.stats++
		if fixture.statErr != nil {
			return nil, fixture.statErr
		}
		return os.Stat(path)
	}
	return fixture
}

// current asks for the user with the given id and fails the test on an error.
func (fixture *currentFixture) current(t *testing.T, id string) (User, bool) {
	t.Helper()
	user, found, err := fixture.users.Current(id)
	if err != nil {
		t.Fatalf("Current(%s): %v", id, err)
	}
	return user, found
}

// wantStats fails the test when the checks of files since the start are not n.
func (fixture *currentFixture) wantStats(t *testing.T, what string, n int) {
	t.Helper()
	if fixture.stats != n {
		t.Errorf("%s: %d checks of the file, want %d", what, fixture.stats, n)
	}
}

func TestUsersCurrent(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [media, docs]\ndisabled: true\n")
	user, found := fixture.current(t, aliceID)
	want := User{ID: aliceID, Name: "alice", PasswordHash: "h", Shares: []string{"media", "docs"}, Disabled: true}
	if !found || !reflect.DeepEqual(user, want) {
		t.Errorf("Current = %+v, %v, want %+v", user, found, want)
	}
	if _, found := fixture.current(t, bobID); found {
		t.Error("found a user that has no file")
	}
	fixture.wantStats(t, "first use", 0)
}

func TestUsersCurrentNeedsAUserID(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	for _, id := range []string{
		"", "alice", "../alice", strings.ToUpper(aliceID), aliceID + ".yaml", "0193f5c2-7d1e-4b2a-9c4d-5e6f7a8b9c0d",
	} {
		fixture.clock.Advance(RecheckInterval)
		if _, found := fixture.current(t, id); found {
			t.Errorf("Current(%q) found a user", id)
		}
	}
	fixture.wantStats(t, "ids that are not user ids", 0)
}

func TestUsersCurrentUsesTheCacheUntilTheIntervalIsOver(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [media]\n")
	fixture.current(t, aliceID)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [docs]\n")

	fixture.clock.Advance(RecheckInterval - time.Nanosecond)
	if user, _ := fixture.current(t, aliceID); !reflect.DeepEqual(user.Shares, []string{"media"}) {
		t.Errorf("within the interval: shares = %v, want the cached [media]", user.Shares)
	}
	fixture.wantStats(t, "within the interval", 0)

	fixture.clock.Advance(time.Nanosecond)
	if user, _ := fixture.current(t, aliceID); !reflect.DeepEqual(user.Shares, []string{"docs"}) {
		t.Errorf("at the interval: shares = %v, want [docs]", user.Shares)
	}
	fixture.wantStats(t, "at the interval", 1)
}

func TestUsersCurrentUnchangedFileIsNotReadAgainAndStartsANewInterval(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	fixture.current(t, aliceID)

	fixture.clock.Advance(RecheckInterval)
	if _, found := fixture.current(t, aliceID); !found {
		t.Fatal("alice not found at the interval")
	}
	fixture.wantStats(t, "first check", 1)
	fixture.clock.Advance(RecheckInterval - time.Second)
	fixture.current(t, aliceID)
	fixture.wantStats(t, "inside the new interval", 1)
	fixture.clock.Advance(time.Second)
	fixture.current(t, aliceID)
	fixture.wantStats(t, "second check", 2)
}

func TestUsersCurrentSeesDisabledRemovedAndAddedUsers(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	writeUser(t, fixture.dir, bobID, "name: bob\npassword_hash: h\n")
	fixture.current(t, aliceID)
	fixture.current(t, bobID)
	if _, found := fixture.current(t, carolID); found {
		t.Fatal("carol has no file yet")
	}
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\ndisabled: true\n")
	if err := os.Remove(filepath.Join(fixture.dir, bobID+".yaml")); err != nil {
		t.Fatal(err)
	}
	writeUser(t, fixture.dir, carolID, "name: carol\npassword_hash: h\n")
	if _, found := fixture.current(t, carolID); found {
		t.Error("within the interval: carol was found before the interval was over")
	}

	fixture.clock.Advance(RecheckInterval)
	if user, found := fixture.current(t, aliceID); !found || !user.Disabled {
		t.Errorf("alice = %+v, %v, want a disabled user", user, found)
	}
	if _, found := fixture.current(t, bobID); found {
		t.Error("bob was removed but is still found")
	}
	if user, found := fixture.current(t, carolID); !found || user.Name != "carol" {
		t.Errorf("carol = %+v, %v, want the added user", user, found)
	}
}

func TestUsersCurrentRemembersThatAUserIsMissing(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	fixture.current(t, aliceID)
	if err := os.Remove(filepath.Join(fixture.dir, aliceID+".yaml")); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(RecheckInterval)
	for range 3 {
		if _, found := fixture.current(t, aliceID); found {
			t.Fatal("alice was removed but is still found")
		}
	}
	fixture.wantStats(t, "a missing user asked for three times", 1)
	fixture.clock.Advance(RecheckInterval)
	fixture.current(t, aliceID)
	fixture.wantStats(t, "a missing user after the interval", 2)
}

func TestUsersCurrentAFolderIsNoUserFile(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.dir, aliceID+".yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.current(t, aliceID)
	fixture.clock.Advance(RecheckInterval)
	if _, found := fixture.current(t, aliceID); found {
		t.Error("a folder named like a user file made a user")
	}
}

func TestUsersCurrentKeepsTheLastGoodContentOfABrokenFile(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: good\n")
	fixture.current(t, aliceID)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: [\n")
	for range 3 {
		fixture.clock.Advance(RecheckInterval)
		if user, found := fixture.current(t, aliceID); !found || user.PasswordHash != "good" {
			t.Fatalf("Current = %+v, %v, want the last good content", user, found)
		}
	}
	// The broken file is read once; checking it again finds the stamp it was read at.
	if got := strings.Count(fixture.logged.String(), "keeping its last good content"); got != 1 {
		t.Errorf("the broken file was warned about %d times, want once:\n%s", got, fixture.logged)
	}
	fixture.wantStats(t, "three intervals", 3)
}

func TestUsersCurrentFailsClosedWhenTheFileCannotBeChecked(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	fixture.current(t, aliceID)
	fixture.clock.Advance(RecheckInterval)
	fixture.statErr = &fs.PathError{Op: "stat", Path: aliceID, Err: fs.ErrPermission}
	for range 2 {
		user, found, err := fixture.users.Current(aliceID)
		if !errors.Is(err, fs.ErrPermission) || found || !reflect.DeepEqual(user, User{}) {
			t.Fatalf("Current = %+v, %v, %v, want no user and the cause of the failure", user, found, err)
		}
	}
	// A failed check is no check: the next call tries again, whatever the interval.
	fixture.wantStats(t, "two failed calls", 2)
	fixture.statErr = nil
	if _, found := fixture.current(t, aliceID); !found {
		t.Error("alice not found once the file can be checked again")
	}
}

func TestUsersLookupFillsTheCache(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [media]\n")
	fixture.users.Lookup("alice")
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [docs]\n")
	fixture.clock.Advance(RecheckInterval - time.Second)
	if user, _ := fixture.current(t, aliceID); !reflect.DeepEqual(user.Shares, []string{"media"}) {
		t.Errorf("shares = %v, want the shares of the login", user.Shares)
	}
	// A login reads the folder again, so it also brings the cache up to date.
	if user, found := fixture.users.Lookup("alice"); !found || !reflect.DeepEqual(user.Shares, []string{"docs"}) {
		t.Fatalf("Lookup = %+v, %v", user, found)
	}
	if user, _ := fixture.current(t, aliceID); !reflect.DeepEqual(user.Shares, []string{"docs"}) {
		t.Errorf("shares = %v after a login, want [docs]", user.Shares)
	}
	fixture.wantStats(t, "logins and requests inside the interval", 0)
}

func TestUsersCurrentOfTheLosingFileOfADuplicateName(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: first\n")
	writeUser(t, fixture.dir, bobID, "name: alice\npassword_hash: second\n")
	if user, found := fixture.current(t, aliceID); !found || user.PasswordHash != "first" {
		t.Errorf("first file: %+v, %v", user, found)
	}
	if _, found := fixture.current(t, bobID); found {
		t.Error("the file that lost the name made a user")
	}
}

func TestUsersCurrentResultIsACopy(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\nshares: [media]\n")
	first, _ := fixture.current(t, aliceID)
	first.Shares[0] = "changed"
	if again, _ := fixture.current(t, aliceID); again.Shares[0] != "media" {
		t.Errorf("shares = %v, want the cache unchanged", again.Shares)
	}
}

func TestUsersCurrentAndLookupTogether(t *testing.T) {
	t.Parallel()
	fixture := newCurrentFixture(t)
	writeUser(t, fixture.dir, aliceID, "name: alice\npassword_hash: h\n")
	start := make(chan struct{})
	var done sync.WaitGroup
	for i := range 16 {
		done.Go(func() {
			<-start
			fixture.clock.Advance(RecheckInterval)
			if i%2 == 0 {
				fixture.users.Lookup("alice")
				return
			}
			if _, _, err := fixture.users.Current(aliceID); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	done.Wait()
}

func TestCheckName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"plain", "alice", nil},
		{"digits first", "1user", nil},
		{"separators inside", "a.b_c-d", nil},
		{"longest", strings.Repeat("a", 64), nil},
		{"empty", "", errEmptyName},
		{"capital letter", "Alice", errBadName},
		{"separator first", ".alice", errBadName},
		{"a space", "al ice", errBadName},
		{"a slash", "a/b", errBadName},
		{"too long", strings.Repeat("a", 65), errBadName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CheckName(tt.input); !errors.Is(got, tt.want) {
				t.Errorf("CheckName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// What MarshalUser writes is what the server reads: the user comes back from the folder the
// way it went in.
func TestMarshalUserIsReadBackByUsers(t *testing.T) {
	t.Parallel()
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2g"
	tests := []struct {
		name string
		user User
	}{
		{"shares", User{ID: aliceID, Name: "alice", PasswordHash: hash, Shares: []string{"media", "docs"}}},
		{"every share", User{ID: bobID, Name: "bob", PasswordHash: hash, Shares: []string{"*"}}},
		{"no share", User{ID: carolID, Name: "carol.x", PasswordHash: hash}},
		{"disabled", User{ID: daveID, Name: "dave", PasswordHash: hash, Shares: []string{"a"}, Disabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalUser(tt.user)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			writeUser(t, dir, tt.user.ID, string(data))
			got, found := newTestUsers(dir).Lookup(tt.user.Name)
			if !found || !reflect.DeepEqual(got, withoutNilShares(tt.user)) {
				t.Errorf("read back %+v, %v, want %+v", got, found, tt.user)
			}
		})
	}
}

// withoutNilShares returns user with an empty share list where it has none, which is how a
// user file with no shares reads back.
func withoutNilShares(user User) User {
	if user.Shares == nil {
		user.Shares = []string{}
	}
	return user
}

func TestMarshalUserLayout(t *testing.T) {
	t.Parallel()
	data, err := MarshalUser(User{Name: "alice", PasswordHash: "$argon2id$x", Shares: []string{"media"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = "name: alice\npassword_hash: $argon2id$x\nshares: [media]\ndisabled: false\n"
	if string(data) != want {
		t.Errorf("MarshalUser = %q, want %q", data, want)
	}
}

func TestMarshalUserRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		user User
		want error
	}{
		{"no name", User{PasswordHash: "h"}, errEmptyName},
		{"bad name", User{Name: "Alice", PasswordHash: "h"}, errBadName},
		{"no hash", User{Name: "alice"}, errEmptyPasswordHash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalUser(tt.user)
			if !errors.Is(err, tt.want) || data != nil {
				t.Errorf("MarshalUser = %q, %v, want %v", data, err, tt.want)
			}
		})
	}
}

func TestCheckShare(t *testing.T) {
	t.Parallel()
	for _, share := range []string{"media", "a.b-c_d", AllShares} {
		if err := CheckShare(share); err != nil {
			t.Errorf("CheckShare(%q) = %v, want nil", share, err)
		}
	}
	for _, share := range []string{"", "Media", "../x", ".hidden", "**"} {
		if err := CheckShare(share); err == nil || !strings.Contains(err.Error(), share) {
			t.Errorf("CheckShare(%q) = %v, want an error that names it", share, err)
		}
	}
}
