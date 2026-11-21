package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/internal/cli"
)

// clusterSeed is the seed of the key that the tests make: the first 32 random bytes. The
// bytes after it make the id of the key.
const clusterSeed = "0123456789abcdef0123456789abcdef"

// testClusterKey returns the cluster key commands with the machine replaced as testStateTool
// has it, and the random bytes that make clusterSeed and testIDBytes.
func testClusterKey() clusterKey {
	return clusterKey{stateTool: testStateTool(strings.NewReader(clusterSeed + testIDBytes))}
}

// stateWithClusterFolder makes a state folder with the cluster folder in it, as a server that
// has started once leaves it, and returns the state folder.
func stateWithClusterFolder(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	if err := os.MkdirAll(app.ClusterDir(state), 0o750); err != nil {
		t.Fatal(err)
	}
	return state
}

// clusterCommand runs a command of cmd with the given words and arguments.
func clusterCommand(t *testing.T, cmd clusterKey, args ...string) outcome {
	t.Helper()
	return executeWith(t, []command{cmd.initCommand(), cmd.showCommand()}, "", args...)
}

// clusterLine is the line that the commands print for the key of the tests: what a client
// needs in its configuration.
func clusterLine() string {
	public := ed25519.NewKeyFromSeed([]byte(clusterSeed))[ed25519.SeedSize:]
	return "cluster_key: " + base64.StdEncoding.EncodeToString(public) + "\n"
}

func keyPath(state string) string { return filepath.Join(app.ClusterDir(state), cluster.KeyFile) }

func TestClusterInitWritesTheKeyFileAndPrintsTheLine(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)

	got := clusterCommand(t, testClusterKey(), "cluster", "init", "--state", state)

	if want := (outcome{stdout: clusterLine()}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if list := fileNames(t, app.ClusterDir(state)); !slices.Equal(list, []string{"key.yaml"}) {
		t.Fatalf("files = %q, want only key.yaml", list)
	}
	info, err := os.Stat(keyPath(state))
	if err != nil || info.Mode() != 0o600 {
		t.Errorf("file mode = %v, %v, want 0600", info.Mode(), err)
	}
	data, err := os.ReadFile(keyPath(state))
	if err != nil {
		t.Fatal(err)
	}
	// Read back with the reader of the server, which is how the file is proven to be in the
	// layout that the server reads.
	made, err := cluster.ParseKey(data)
	if err != nil || !strings.HasSuffix(clusterLine(), made.PublicText()+"\n") {
		t.Errorf("read back %+v, %v, want the key of the line %q", made, err, clusterLine())
	}
	if want := testID(t); made.ID != want {
		t.Errorf("id = %q, want %q", made.ID, want)
	}
}

func TestClusterInitRefusesASecondKeyAndLeavesTheFirst(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	cmd := testClusterKey()
	if first := clusterCommand(t, cmd, "cluster", "init", "--state", state); first.status != cli.ExitOK {
		t.Fatalf("first init: %+v", first)
	}
	before, err := os.ReadFile(keyPath(state))
	if err != nil {
		t.Fatal(err)
	}

	got := clusterCommand(t, testClusterKey(), "cluster", "init", "--state", state)

	if want := (outcome{status: cli.ExitError, stderr: "splicefsdctl: this cluster already has a key\n"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	after, err := os.ReadFile(keyPath(state))
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("key file after the refusal = %q, %v, want %q", after, err, before)
	}
	if list := fileNames(t, app.ClusterDir(state)); !slices.Equal(list, []string{"key.yaml"}) {
		t.Errorf("files = %q, want only key.yaml", list)
	}
}

func TestClusterShowPrintsTheLineAgain(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	cmd := testClusterKey()
	if first := clusterCommand(t, cmd, "cluster", "init", "--state", state); first.status != cli.ExitOK {
		t.Fatalf("init: %+v", first)
	}

	got := clusterCommand(t, cmd, "cluster", "show", "--state", state)

	if want := (outcome{stdout: clusterLine()}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestClusterCommandsFindTheDefaultStateFolder(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	cmd := testClusterKey()
	cmd.defaultState = func() (string, error) { return state, nil }
	if got := clusterCommand(t, cmd, "cluster", "init"); got.status != cli.ExitOK {
		t.Fatalf("init: %+v", got)
	}
	if got, want := clusterCommand(t, cmd, "cluster", "show"), (outcome{stdout: clusterLine()}); got != want {
		t.Errorf("show: got %+v, want %+v", got, want)
	}
}

func TestClusterCommandsWithoutAStateFolderToBeFound(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"init", "show"} {
		got := clusterCommand(t, testClusterKey(), "cluster", word)
		if want := (outcome{status: cli.ExitError, stderr: "splicefsdctl: no home directory\n"}); got != want {
			t.Errorf("%s: got %+v, want %+v", word, got, want)
		}
	}
}

func TestClusterKeyFileHasTheOwnerOfTheFolder(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	if got := clusterCommand(t, testClusterKey(), "cluster", "init", "--state", state); got.status != cli.ExitOK {
		t.Fatalf("got %+v", got)
	}
	folder, file := statOf(t, app.ClusterDir(state)), statOf(t, keyPath(state))
	if file.Uid != folder.Uid || file.Gid != folder.Gid {
		t.Errorf("file owner %d:%d, folder owner %d:%d", file.Uid, file.Gid, folder.Uid, folder.Gid)
	}
}

// Run as root, the file starts as root's and must end as the folder's, whoever that is: this is
// how a key made by root is readable by the user the server runs as, and by nobody else.
func TestClusterInitAsRootGivesTheFileToTheOwnerOfTheFolder(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("changing the owner of a file needs root")
	}
	const uid, gid = 4242, 4343
	state := stateWithClusterFolder(t)
	if err := os.Chown(app.ClusterDir(state), uid, gid); err != nil {
		t.Fatal(err)
	}
	if got := clusterCommand(t, testClusterKey(), "cluster", "init", "--state", state); got.status != cli.ExitOK {
		t.Fatalf("got %+v", got)
	}
	file := statOf(t, keyPath(state))
	if file.Uid != uid || file.Gid != gid {
		t.Errorf("file owner %d:%d, want %d:%d", file.Uid, file.Gid, uid, gid)
	}
}

// A key file that cannot be given to the owner of the folder is removed: left in place it
// would be a key that the server cannot read, and that init then refuses to replace.
func TestClusterInitRemovesTheFileItCouldNotGiveAway(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	cmd := testClusterKey()
	cmd.chown = func(string, int, int) error { return errTestChown }

	got := clusterCommand(t, cmd, "cluster", "init", "--state", state)

	if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, "operation not permitted") ||
		!strings.Contains(got.stderr, "give the cluster key file to the owner of the cluster folder") {
		t.Errorf("got %+v", got)
	}
	if list := fileNames(t, app.ClusterDir(state)); len(list) != 0 {
		t.Errorf("files = %q, want none", list)
	}
}

func TestClusterCommandUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"init with a word after it", []string{"init", "now"}, "cluster init takes no arguments"},
		{"show with a word after it", []string{"show", "now"}, "cluster show takes no arguments"},
		{"init with a flag after a word", []string{"init", "now", "--state", "x"}, "cluster init takes no arguments"},
		{"init with an unknown flag", []string{"init", "--bogus"}, "flag provided but not defined: -bogus"},
		{"show with an unknown flag", []string{"show", "--bogus"}, "flag provided but not defined: -bogus"},
		{"init with a state without its value", []string{"init", "--state"}, "flag needs an argument: -state"},
		{"show with a state without its value", []string{"show", "--state"}, "flag needs an argument: -state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := clusterCommand(t, testClusterKey(), append([]string{"cluster"}, tt.args...)...)
			if got.status != cli.ExitUsage || got.stdout != "" ||
				!strings.HasPrefix(got.stderr, "splicefsdctl: "+tt.want+"\n") {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestClusterCommandHelp(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"init", "show"} {
		for _, arg := range []string{"-h", "--help"} {
			got := execute(t, "cluster", word, arg)
			if want := (outcome{status: cli.ExitOK, stdout: fullUsage}); got != want {
				t.Errorf("%s %s: got %+v, want %+v", word, arg, got, want)
			}
		}
	}
}

// What init refuses, it refuses before it makes the key, and it leaves nothing behind.
func TestClusterInitRefusals(t *testing.T) {
	t.Parallel()
	notAFolder := func(folder string) string { return "cluster folder " + folder + " is not a folder" }
	tests := []struct {
		name  string
		setup setup
		want  func(folder string) string
	}{
		{"no cluster folder", noFolder, func(string) string { return "cluster folder: " }},
		{"a file where the cluster folder belongs", fileForFolder, notAFolder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := stateWithClusterFolder(t)
			folder := app.ClusterDir(state)
			tt.setup(t, folder)
			cmd := testClusterKey()
			cmd.random = readFails{t}

			got := clusterCommand(t, cmd, "cluster", "init", "--state", state)

			want := tt.want(folder)
			if got.status != cli.ExitError || got.stdout != "" ||
				!strings.HasPrefix(got.stderr, "splicefsdctl: ") || !strings.Contains(got.stderr, want) {
				t.Errorf("got %+v, want status 1 with %q", got, want)
			}
		})
	}
}

// readFails is a random source that fails the test when it is read: a refusal comes first.
type readFails struct{ t *testing.T }

func (r readFails) Read([]byte) (int, error) {
	r.t.Error("random bytes were read before the refusal")
	return 0, io.EOF
}

func TestClusterInitWithoutRandomBytes(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	cmd := testClusterKey()
	cmd.random = strings.NewReader(clusterSeed[:7])

	got := clusterCommand(t, cmd, "cluster", "init", "--state", state)

	if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, "read random bytes") {
		t.Errorf("got %+v", got)
	}
	if list := fileNames(t, app.ClusterDir(state)); len(list) != 0 {
		t.Errorf("files = %q, want none", list)
	}
}

func TestClusterShowWithoutAKey(t *testing.T) {
	t.Parallel()
	for name, state := range map[string]string{
		"no key file":       stateWithClusterFolder(t),
		"no cluster folder": t.TempDir(),
		"no state folder":   filepath.Join(t.TempDir(), "absent"),
	} {
		got := clusterCommand(t, testClusterKey(), "cluster", "show", "--state", state)
		want := outcome{
			status: cli.ExitError,
			stderr: "splicefsdctl: this cluster has no key, run splicefsdctl cluster init\n",
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

// badPrivateKey is a key file whose private key is not the base64 of 32 bytes.
const badPrivateKey = "id: 0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0d\nprivate_key: x\npublic_key: y\n"

func TestClusterShowRefusesAFileThatIsNotAKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"not a key file", "hello\n", "key.yaml must be YAML with the keys id, private_key and public_key"},
		{"a bad private key", badPrivateKey, "private_key must be the base64 of 32 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := stateWithClusterFolder(t)
			if err := os.WriteFile(keyPath(state), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got := clusterCommand(t, testClusterKey(), "cluster", "show", "--state", state)

			if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, tt.want) ||
				!strings.Contains(got.stderr, keyPath(state)) {
				t.Errorf("got %+v, want status 1 naming %s with %q", got, keyPath(state), tt.want)
			}
		})
	}
}

func TestClusterShowWithAKeyFileThatCannotBeRead(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	if err := os.Mkdir(keyPath(state), 0o700); err != nil {
		t.Fatal(err)
	}

	got := clusterCommand(t, testClusterKey(), "cluster", "show", "--state", state)

	if got.status != cli.ExitError || got.stdout != "" || !strings.Contains(got.stderr, "read the cluster key file") {
		t.Errorf("got %+v", got)
	}
}

func TestClusterInitReportsAFailedWriteOfTheLine(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	var errOut strings.Builder
	job := call{
		self: cli.Program{Name: program, Usage: ""}, args: []string{"--state", state},
		stdin: strings.NewReader(""), stdout: errWriter{}, stderr: &errOut,
	}
	status := testClusterKey().runInit(job)
	if want := "splicefsdctl: write the line: disk full\n"; status != cli.ExitError || errOut.String() != want {
		t.Errorf("status %d stderr %q, want %q", status, errOut.String(), want)
	}
}

func TestClusterShowReportsAFailedWriteOfTheLine(t *testing.T) {
	t.Parallel()
	state := stateWithClusterFolder(t)
	if got := clusterCommand(t, testClusterKey(), "cluster", "init", "--state", state); got.status != cli.ExitOK {
		t.Fatalf("init: %+v", got)
	}
	var errOut strings.Builder
	job := call{
		self: cli.Program{Name: program, Usage: ""}, args: []string{"--state", state},
		stdin: strings.NewReader(""), stdout: errWriter{}, stderr: &errOut,
	}
	status := testClusterKey().runShow(job)
	if want := "splicefsdctl: write the line: disk full\n"; status != cli.ExitError || errOut.String() != want {
		t.Errorf("status %d stderr %q, want %q", status, errOut.String(), want)
	}
}
