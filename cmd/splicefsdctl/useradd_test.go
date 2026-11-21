package main

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/internal/cli"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// The time and the random bytes of the tests, which make the id that user add prints: the first
// 16 bytes are for a salt, as many as the hash of a password takes, and the rest make the id.
var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // a constant of the tests

const (
	testSalt     = "0123456789abcdef"
	testIDBytes  = "ghijklmnop"
	testHash     = "$argon2id$stub"
	testPassword = "correct horse"
)

var errNoState = errors.New("no home directory")

// testID is the id that user add makes from testNow and testIDBytes.
func testID(t *testing.T) string {
	t.Helper()
	id, err := uuid7.New(testNow, strings.NewReader(testIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// stubHash makes a hash without the cost of a real one. It reads the salt bytes as the real one
// does, so what is left for the id is what the real one would leave.
func stubHash(in, random io.Reader) (string, error) {
	if _, err := readPassword(in); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(random, make([]byte, len(testSalt))); err != nil {
		return "", err
	}
	return testHash, nil
}

// testUserAdd returns a user add that knows no default state folder, hashes with the stub and
// has a fixed time and fixed random bytes.
func testUserAdd() userAdd {
	return userAdd{
		defaultState: func() (string, error) { return "", errNoState },
		hash:         stubHash,
		now:          func() time.Time { return testNow },
		random:       strings.NewReader(testSalt + testIDBytes),
		chown:        os.Chown,
	}
}

// stateWithUsers makes a state folder with the folder of the users in it and returns the state
// folder. The folder of the users is app.UsersDir of it.
func stateWithUsers(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	if err := os.MkdirAll(app.UsersDir(state), 0o750); err != nil {
		t.Fatal(err)
	}
	return state
}

// addUser runs user add of cmd with the given input and arguments after "user add".
func addUser(t *testing.T, cmd userAdd, input string, args ...string) outcome {
	t.Helper()
	return executeWith(t, []command{cmd.command()}, input, append([]string{"user", "add"}, args...)...)
}

// fileNames lists the files in dir.
func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		list = append(list, item.Name())
	}
	return list
}

func TestUserAddWritesTheUserFile(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	id := testID(t)

	got := addUser(t, testUserAdd(), testPassword+"\n",
		"--state", state, "--share", "media", "--share", "docs", "alice")

	if want := (outcome{stdout: id + "\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	path := filepath.Join(users, id+".yaml")
	if list := fileNames(t, users); len(list) != 1 || list[0] != id+".yaml" {
		t.Fatalf("files = %q, want only %s.yaml", list, id)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode() != 0o640 {
		t.Errorf("file mode = %v, %v, want 0640", info.Mode(), err)
	}
	want := auth.User{ID: id, Name: "alice", PasswordHash: testHash, Shares: []string{"media", "docs"}}
	if got, found := readBack(users, "alice"); !found || !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, %v, want %+v", got, found, want)
	}
}

// readBack finds the user in the folder with the reader of the server, which is how the file
// is proven to be in the layout the server reads.
func readBack(users, name string) (auth.User, bool) {
	return auth.NewUsers(users, time.Now, slog.New(slog.DiscardHandler)).Lookup(name)
}

// With nothing stubbed, the password that is typed is the password the hash verifies.
func TestUserAddHashesThePasswordLikeHashPassword(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	var out, errOut strings.Builder
	status := run([]string{"user", "add", "--state", state, "alice"},
		strings.NewReader(testPassword+"\n"), &out, &errOut)
	if status != cli.ExitOK || errOut.Len() != 0 {
		t.Fatalf("status %d stderr %q", status, errOut.String())
	}
	id := strings.TrimSuffix(out.String(), "\n")
	if !uuid7.Valid(id) {
		t.Fatalf("stdout = %q, want an id", out.String())
	}
	user, found := readBack(users, "alice")
	if !found || user.ID != id {
		t.Fatalf("read back %+v, %v", user, found)
	}
	if ok, err := auth.VerifyPassword(user.PasswordHash, testPassword); err != nil || !ok {
		t.Errorf("the hash does not verify the password: %v, %v", ok, err)
	}
}

func TestUserAddFileHasTheOwnerOfTheFolder(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	id := testID(t)
	if got := addUser(t, testUserAdd(), "pw\n", "--state", state, "alice"); got.status != cli.ExitOK {
		t.Fatalf("got %+v", got)
	}
	folder, file := statOf(t, users), statOf(t, filepath.Join(users, id+".yaml"))
	if file.Uid != folder.Uid || file.Gid != folder.Gid {
		t.Errorf("file owner %d:%d, folder owner %d:%d", file.Uid, file.Gid, folder.Uid, folder.Gid)
	}
}

// Run as root, the file starts as root's and must end as the folder's, whoever that is: this is
// how a file made by root is readable by the user the server runs as.
func TestUserAddAsRootGivesTheFileToTheOwnerOfTheFolder(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("changing the owner of a file needs root")
	}
	const uid, gid = 4242, 4343
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	if err := os.Chown(users, uid, gid); err != nil {
		t.Fatal(err)
	}
	id := testID(t)
	if got := addUser(t, testUserAdd(), "pw\n", "--state", state, "alice"); got.status != cli.ExitOK {
		t.Fatalf("got %+v", got)
	}
	file := statOf(t, filepath.Join(users, id+".yaml"))
	if file.Uid != uid || file.Gid != gid {
		t.Errorf("file owner %d:%d, want %d:%d", file.Uid, file.Gid, uid, gid)
	}
}

// statOf returns the owner fields of path.
func statOf(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no owner in the file information")
	}
	return st
}

func TestUserAddFindsTheDefaultStateFolder(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	cmd := testUserAdd()
	cmd.defaultState = func() (string, error) { return state, nil }
	if got := addUser(t, cmd, "pw\n", "alice"); got.status != cli.ExitOK {
		t.Fatalf("got %+v", got)
	}
	if list := fileNames(t, users); len(list) != 1 {
		t.Errorf("files = %q, want the new user file", list)
	}
}

func TestUserAddWithoutAStateFolderToBeFound(t *testing.T) {
	t.Parallel()
	got := addUser(t, testUserAdd(), "pw\n", "alice")
	if want := (outcome{status: cli.ExitError, stderr: "splicefsdctl: no home directory\n"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUserAddUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no name", nil, "user add takes one name"},
		{"two names", []string{"alice", "bob"}, "user add takes one name"},
		{"flags after the name", []string{"alice", "--share", "media"}, "user add takes one name"},
		{"unknown flag", []string{"--bogus", "alice"}, "flag provided but not defined: -bogus"},
		{"a flag after the name", []string{"alice", "--state"}, "user add takes one name"},
		{"state without its value", []string{"--state"}, "flag needs an argument: -state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := addUser(t, testUserAdd(), "pw\n", tt.args...)
			if got.status != cli.ExitUsage || got.stdout != "" ||
				!strings.HasPrefix(got.stderr, "splicefsdctl: "+tt.want+"\n") {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestUserAddHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		got := execute(t, "user", "add", arg)
		if want := (outcome{status: cli.ExitOK, stdout: fullUsage}); got != want {
			t.Errorf("%s: got %+v, want %+v", arg, got, want)
		}
	}
}

// A setup prepares the state of a test of a refusal. It is given the folder of the users.
type setup func(t *testing.T, users string)

// existingUser writes the file of a user named alice, as an earlier user add would.
func existingUser(t *testing.T, users string) {
	t.Helper()
	data, err := auth.MarshalUser(auth.User{Name: "alice", PasswordHash: testHash})
	if err != nil {
		t.Fatal(err)
	}
	const earlierID = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0d"
	if err := datadir.WriteOnce(filepath.Join(users, earlierID+".yaml"), data, 0o640); err != nil {
		t.Fatal(err)
	}
}

// noUsersFolder removes the folder of the users.
func noUsersFolder(t *testing.T, users string) {
	t.Helper()
	if err := os.Remove(users); err != nil {
		t.Fatal(err)
	}
}

// fileForUsersFolder puts a file where the folder of the users belongs.
func fileForUsersFolder(t *testing.T, users string) {
	t.Helper()
	noUsersFolder(t, users)
	if err := os.WriteFile(users, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// What user add refuses, it refuses before it reads the password, and it leaves nothing behind.
func TestUserAddRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup setup
		given string
		want  string
	}{
		{"empty name", nil, "", "name is empty"},
		{"capital letters", nil, "Alice", "name must match ^[a-z0-9][a-z0-9._-]{0,63}$"},
		{"a path", nil, "../x", "name must match"},
		{"name of another user", existingUser, "alice", `user "alice" already exists`},
		{"no users folder", noUsersFolder, "alice", "users folder: "},
		{"a file where the users folder belongs", fileForUsersFolder, "alice", "is not a folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := stateWithUsers(t)
			users := app.UsersDir(state)
			if tt.setup != nil {
				tt.setup(t, users)
			}
			before, _ := os.ReadDir(users) //nolint:errcheck // the folder is missing in some cases
			cmd := testUserAdd()
			cmd.hash = func(io.Reader, io.Reader) (string, error) {
				t.Error("the password was read before the refusal")
				return testHash, nil
			}

			got := addUser(t, cmd, "pw\n", "--state", state, tt.given)

			if got.status != cli.ExitError || got.stdout != "" ||
				!strings.HasPrefix(got.stderr, "splicefsdctl: ") || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("got %+v, want status 1 with %q", got, tt.want)
			}
			after, _ := os.ReadDir(users) //nolint:errcheck // the folder is missing in some cases
			if len(after) != len(before) {
				t.Errorf("files before %d, after %d", len(before), len(after))
			}
		})
	}
}

func TestUserAddRefusesABadShareName(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	cmd := testUserAdd()
	cmd.hash = func(io.Reader, io.Reader) (string, error) {
		t.Error("the password was read before the refusal")
		return testHash, nil
	}

	got := addUser(t, cmd, "pw\n", "--state", state, "--share", "Media", "alice")

	if got.status == cli.ExitOK || got.stdout != "" || !strings.Contains(got.stderr, `share "Media"`) {
		t.Errorf("got %+v, want a refusal that names the share", got)
	}
	if files, err := os.ReadDir(app.UsersDir(state)); err != nil || len(files) != 0 {
		t.Errorf("users folder after the refusal: %d files, %v", len(files), err)
	}
}

func TestUserAddTakesEveryShare(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)

	got := addUser(t, testUserAdd(), "pw\n", "--state", state, "--share", auth.AllShares, "alice")

	if got.status != cli.ExitOK {
		t.Fatalf("got %+v, want success", got)
	}
}

var errTestChown = errors.New("operation not permitted")

// idTaken makes a file where the id that user add will make belongs, so that writing it fails
// for anyone, root included.
func idTaken(t *testing.T, users string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(users, testID(t)+".yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUserAddFailures(t *testing.T) {
	t.Parallel()
	hashes := func(hash func(in, random io.Reader) (string, error)) func(*userAdd) {
		return func(cmd *userAdd) { cmd.hash = hash }
	}
	tests := []struct {
		name   string
		setup  setup
		change func(*userAdd)
		want   string
	}{
		// The real hash refuses an empty password before it spends any time on a hash.
		{"empty password", nil, hashes(passwordHash), "hash password: empty password"},
		{
			"unreadable password", nil,
			hashes(func(io.Reader, io.Reader) (string, error) { return "", errTerminalGone }), "terminal gone",
		},
		{
			"no random bytes for the id", nil,
			func(cmd *userAdd) { cmd.random = strings.NewReader(testSalt) }, "read random bytes",
		},
		{
			"a hash the server would not read", nil,
			hashes(func(io.Reader, io.Reader) (string, error) { return "", nil }), "password_hash is empty",
		},
		{"the id is taken", idTaken, nil, "write user file: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := stateWithUsers(t)
			users := app.UsersDir(state)
			cmd := testUserAdd()
			if tt.setup != nil {
				tt.setup(t, users)
			}
			if tt.change != nil {
				tt.change(&cmd)
			}
			before := fileNames(t, users)

			got := addUser(t, cmd, "\n", "--state", state, "alice")

			if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("got %+v, want status 1 with %q", got, tt.want)
			}
			if after := fileNames(t, users); !slices.Equal(after, before) {
				t.Errorf("files before %q, after %q", before, after)
			}
		})
	}
}

// A file that cannot be given to the owner of the folder is removed: left in place it would be
// a user file that the server cannot read.
func TestUserAddRemovesTheFileItCouldNotGiveAway(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	users := app.UsersDir(state)
	cmd := testUserAdd()
	cmd.chown = func(string, int, int) error { return errTestChown }

	got := addUser(t, cmd, "pw\n", "--state", state, "alice")

	if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, "operation not permitted") {
		t.Errorf("got %+v", got)
	}
	if list := fileNames(t, users); len(list) != 0 {
		t.Errorf("files = %q, want none", list)
	}
}

func TestUserAddReportsAFailedWriteOfTheId(t *testing.T) {
	t.Parallel()
	state := stateWithUsers(t)
	var errOut strings.Builder
	job := call{
		self: cli.Program{Name: program, Usage: ""}, args: []string{"--state", state, "alice"},
		stdin: strings.NewReader("pw\n"), stdout: errWriter{}, stderr: &errOut,
	}
	status := testUserAdd().run(job)
	if want := "splicefsdctl: write id: disk full\n"; status != cli.ExitError || errOut.String() != want {
		t.Errorf("status %d stderr %q, want %q", status, errOut.String(), want)
	}
}

type fakeInfo struct{ fs.FileInfo }

func (fakeInfo) Sys() any { return nil }

func TestOwnerOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := statOf(t, dir)
	if got, err := ownerOf(info); err != nil || got.uid != int(st.Uid) || got.gid != int(st.Gid) {
		t.Errorf("ownerOf = %+v, %v, want %d:%d", got, err, st.Uid, st.Gid)
	}
	if _, err := ownerOf(fakeInfo{info}); !errors.Is(err, errNoFolderOwner) {
		t.Errorf("ownerOf without owner = %v", err)
	}
}
