package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/internal/datadir"
)

// folderMode is the mode that the tests ask makeFolder for.
const folderMode = 0o750

// folderSetup prepares the path of a folder before makeFolder runs.
type folderSetup func(t *testing.T, path string)

// prepareFolderPath returns a path in a new directory, after setup has run on it.
func prepareFolderPath(t *testing.T, setup folderSetup) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "folder")
	if setup != nil {
		setup(t, path)
	}
	return path
}

func TestMakeFolderLeavesAFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup folderSetup
		mode  os.FileMode
	}{
		{
			name: "missing folder is made with exactly its mode",
			mode: folderMode,
		},
		{
			name: "existing folder keeps its mode",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			mode: 0o700,
		},
		{
			name: "link to a folder is accepted",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			mode: 0o700,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := prepareFolderPath(t, tt.setup)
			if err := makeFolder(folder{path, folderMode}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() || info.Mode().Perm() != tt.mode {
				t.Errorf("folder = %v, %v, want a folder with mode %v", info, err, tt.mode)
			}
		})
	}
}

func TestMakeFolderRefusesWhatIsNotAFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup folderSetup
		want  string // part of the expected error
	}{
		{
			name: "file where the folder belongs",
			setup: func(t *testing.T, path string) {
				t.Helper()
				write(t, filepath.Dir(path), filepath.Base(path), "a file")
			},
			want: "is not a folder",
		},
		{
			name: "link to nothing where the folder belongs",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(filepath.Dir(path), "nowhere"), path); err != nil {
					t.Fatal(err)
				}
			},
			want: "no such file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := prepareFolderPath(t, tt.setup)
			err := makeFolder(folder{path, folderMode})
			if err == nil || !strings.Contains(err.Error(), tt.want) ||
				!strings.Contains(err.Error(), path) {
				t.Fatalf("makeFolder = %v, want an error naming %s and containing %q",
					err, path, tt.want)
			}
		})
	}
}

// fixedNow is the clock of the node id tests: the time of the UUIDv7 example in RFC 9562.
func fixedNow() time.Time { return time.UnixMilli(0x017F22E279B0) }

// rfcRandom is the random source that, with fixedNow, gives the UUIDv7 example of RFC 9562.
func rfcRandom() io.Reader {
	return bytes.NewReader([]byte{0xFC, 0xC3, 0xD8, 0xC4, 0xDC, 0x0C, 0x0C, 0x07, 0x39, 0x8F})
}

const rfcNodeID = "017f22e2-79b0-7cc3-98c4-dc0c0c07398f"

// Ids of the users of the tests, and the files that are named by them.
const (
	aliceID   = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0d"
	carolID   = "0193f5c2-7d1e-7b2a-9c4d-5e6f7a8b9c0f"
	aliceFile = aliceID + ".yaml"
	carolFile = carolID + ".yaml"
)

func TestNodeIDIsMadeOnceAndKept(t *testing.T) {
	t.Parallel()
	paths := layout(t.TempDir())
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	id, err := paths.nodeID(fixedNow, rfcRandom())
	if err != nil || id != rfcNodeID {
		t.Fatalf("first nodeID = %q, %v, want %q", id, err, rfcNodeID)
	}
	saved, err := os.ReadFile(paths.nodeIDFile)
	if err != nil || string(saved) != rfcNodeID+"\n" {
		t.Errorf("saved node id = %q, %v, want %q", saved, err, rfcNodeID+"\n")
	}
	info, err := os.Stat(paths.nodeIDFile)
	if err != nil || info.Mode().Perm() != nodeIDMode {
		t.Errorf("node id file = %v, %v, want mode %o", info, err, nodeIDMode)
	}
	// The second start reads the file: neither the clock nor the random source is used.
	again, err := paths.nodeID(func() time.Time { return time.Time{} }, iotest.ErrReader(errors.New("unused")))
	if err != nil || again != rfcNodeID {
		t.Errorf("second nodeID = %q, %v, want %q", again, err, rfcNodeID)
	}
}

// rival is a random source that plays another process starting at the same time: when this
// start asks for random bytes, the other start has just saved its own node id.
type rival struct {
	t    *testing.T
	path string
	id   string
}

func (r rival) Read(p []byte) (int, error) {
	if err := os.WriteFile(r.path, []byte(r.id+"\n"), nodeIDMode); err != nil {
		r.t.Error(err)
	}
	clear(p)
	return len(p), nil
}

// The file is saved by whichever start gets there first, and the other start uses that id:
// a second id would make this one node two.
func TestNodeIDOfTheStartThatSavedFirstWins(t *testing.T) {
	t.Parallel()
	paths := layout(t.TempDir())
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	const other = "00000000-0000-7000-8000-000000000001"
	id, err := paths.nodeID(fixedNow, rival{t, paths.nodeIDFile, other})
	if err != nil || id != other {
		t.Errorf("nodeID = %q, %v, want %q", id, err, other)
	}
	if saved, readErr := os.ReadFile(paths.nodeIDFile); readErr != nil || string(saved) != other+"\n" {
		t.Errorf("saved node id = %q, %v, want %q", saved, readErr, other+"\n")
	}
}

// Starts that overlap each make an id of their own and one of them is saved: all of them
// must return that one. The start signal releases them together.
func TestNodeIDIsOneForStartsAtOnce(t *testing.T) {
	t.Parallel()
	const starts = 8
	paths := layout(t.TempDir())
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	begin := make(chan struct{})
	ids := make([]string, starts)
	errs := make([]error, starts)
	var done sync.WaitGroup
	for i := range starts {
		done.Go(func() {
			<-begin
			random := bytes.NewReader(bytes.Repeat([]byte{byte(i + 1)}, 10))
			ids[i], errs[i] = paths.nodeID(fixedNow, random)
		})
	}
	close(begin)
	done.Wait()

	saved, err := os.ReadFile(paths.nodeIDFile)
	if err != nil {
		t.Fatal(err)
	}
	for i := range starts {
		if errs[i] != nil || ids[i]+"\n" != string(saved) {
			t.Errorf("start %d: %q, %v, want the saved id %q", i, ids[i], errs[i], saved)
		}
	}
}

func TestNodeIDErrors(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	tests := []struct {
		name   string
		setup  func(t *testing.T, paths dataPaths)
		random io.Reader // nil is randomness that works
		want   string
	}{
		{
			name:   "no randomness",
			random: iotest.ErrReader(failing),
			want:   "make node id: read random bytes: injected",
		},
		{
			name: "file holds text that is not an id",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				writeNodeIDFile(t, paths, "not an id\n")
			},
			want: "must hold a UUIDv7",
		},
		{
			// An id of an earlier version is refused, never replaced: other nodes know it.
			name: "file holds an id of 16 hexadecimal characters",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				writeNodeIDFile(t, paths, "0123456789abcdef\n")
			},
			want: "must hold a UUIDv7",
		},
		{
			name: "file holds a UUIDv7 in upper case",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				writeNodeIDFile(t, paths, strings.ToUpper(rfcNodeID)+"\n")
			},
			want: "must hold a UUIDv7",
		},
		{
			name: "node id path is a folder",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				if err := os.MkdirAll(paths.nodeIDFile, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "read node id file",
		},
		{
			// The folder for the file does not exist: reading says the file is missing and
			// writing fails. A read-only folder would not stop root, which runs the tests
			// on Linux.
			name: "node id cannot be written",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				if err := os.RemoveAll(filepath.Dir(paths.nodeIDFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: "write node id file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			paths := layout(t.TempDir())
			if tt.setup != nil {
				tt.setup(t, paths)
			}
			random := tt.random
			if random == nil {
				random = zeros{}
			}
			id, err := paths.nodeID(fixedNow, random)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("nodeID = %q, %v, want an error containing %q", id, err, tt.want)
			}
		})
	}
}

// writeNodeIDFile saves content as the node id file, as an earlier start would have.
func writeNodeIDFile(t *testing.T, paths dataPaths, content string) {
	t.Helper()
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.nodeIDFile, []byte(content), nodeIDMode); err != nil {
		t.Fatal(err)
	}
}

// zeros is a source of randomness that always works, so that a test fails at the place it
// aims at and not in the randomness.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// failingNodes is a node list that cannot be read.
type failingNodes struct{ err error }

func (f failingNodes) Nodes() (wire.Nodes, error) { return wire.Nodes{}, f.err }

func TestNamedClusterWrapsTheErrorOfTheNodeList(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	cluster := namedCluster{name: "example", nodes: failingNodes{err: failing}}
	nodes, err := cluster.Nodes()
	if !errors.Is(err, failing) || !strings.Contains(err.Error(), "list nodes") {
		t.Errorf("Nodes error = %v, want list nodes wrapping the injected error", err)
	}
	if nodes.Cluster != "example" {
		t.Errorf("cluster = %q, want the configured name", nodes.Cluster)
	}
}

// anotherNodeID is the id of a node other than rfcNodeID.
const anotherNodeID = "017f22e2-79b0-7cc3-98c4-dc0c0c073990"

// tooLong returns a path below dir whose last name is longer than any file system allows, so
// that every call on it fails whoever runs the test, root included.
func tooLong(dir string) string {
	return filepath.Join(dir, strings.Repeat("k", 300))
}

func TestRemoveOldKeys(t *testing.T) {
	t.Parallel()
	own, other := datadir.IDFile(rfcNodeID), datadir.IDFile(anotherNodeID)
	tests := []struct {
		name string
		// files are the files of the old keys folder; nil means that there is no folder.
		files []string
		// left are the files that stay; nil means that the folder is gone too.
		left []string
	}{
		{name: "no folder"},
		{name: "own file only", files: []string{own}},
		{name: "empty folder", files: []string{}},
		{name: "own file and the file of another node", files: []string{own, other}, left: []string{other}},
		{name: "file of another node only", files: []string{other}, left: []string{other}},
		{name: "own file and a stray file", files: []string{own, "notes.txt"}, left: []string{"notes.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := layout(t.TempDir())
			fillOldKeys(t, data, tt.files)
			if err := data.removeOldKeys(rfcNodeID); err != nil {
				t.Fatal(err)
			}
			names, exists := listed(t, data.oldKeysDir)
			if exists != (tt.left != nil) || !reflect.DeepEqual(names, tt.left) {
				t.Errorf("old keys folder holds %v (exists: %v), want %v", names, exists, tt.left)
			}
		})
	}
}

// fillOldKeys makes the old keys folder with files in it; nil files make no folder.
func fillOldKeys(t *testing.T, data dataPaths, files []string) {
	t.Helper()
	if files == nil {
		return
	}
	if err := os.MkdirAll(data.oldKeysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		write(t, data.oldKeysDir, name, "keys: []\n")
	}
}

// listed returns the names in dir, and false if there is no such folder.
func listed(t *testing.T, dir string) ([]string, bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, true
}

func TestRemoveOldKeysTwiceIsTheSameAsOnce(t *testing.T) {
	t.Parallel()
	data := layout(t.TempDir())
	write(t, data.oldKeysDir, datadir.IDFile(rfcNodeID), "keys: []\n")
	for range 2 {
		if err := data.removeOldKeys(rfcNodeID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveOldKeysLeavesAFileAtTheFolderPathAlone(t *testing.T) {
	t.Parallel()
	data := layout(t.TempDir())
	path := write(t, filepath.Dir(data.oldKeysDir), filepath.Base(data.oldKeysDir), "not this server's")
	if err := data.removeOldKeys(rfcNodeID); err != nil {
		t.Errorf("removeOldKeys = %v, want nil: the path is not used any more", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "not this server's" {
		t.Errorf("file at the folder path after removeOldKeys = %q, %v; want it unchanged", got, err)
	}
}

func TestRemoveOldKeysFailsWhenItsOwnFileCannotBeRemoved(t *testing.T) {
	t.Parallel()
	data := layout(t.TempDir())
	own := filepath.Join(data.oldKeysDir, datadir.IDFile(rfcNodeID))
	write(t, own, "inside", "a folder where the node's file belongs")
	err := data.removeOldKeys(rfcNodeID)
	if err == nil || !strings.Contains(err.Error(), "remove old token keys") {
		t.Errorf("removeOldKeys = %v, want an error that says what was being removed", err)
	}
}

func TestRemoveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := write(t, dir, "there", "x")
	for range 2 {
		if err := removeFile(path); err != nil {
			t.Errorf("removeFile = %v", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file after removeFile: %v", err)
	}
	if err := removeFile(tooLong(dir)); err == nil {
		t.Error("removeFile of a name that is too long = nil, want an error")
	}
}

func TestRemoveEmptyFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty, full := filepath.Join(dir, "empty"), filepath.Join(dir, "full")
	for _, path := range []string{empty, full} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, full, "file", "x")
	for _, path := range []string{empty, full, filepath.Join(dir, "absent")} {
		if err := removeEmptyFolder(path); err != nil {
			t.Errorf("removeEmptyFolder(%s) = %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty folder after removeEmptyFolder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(full, "file")); err != nil {
		t.Errorf("file in the full folder after removeEmptyFolder: %v", err)
	}
	if err := removeEmptyFolder(tooLong(dir)); err == nil {
		t.Error("removeEmptyFolder of a name that is too long = nil, want an error")
	}
}

// UsersDir is where the server reads the user files, so that a tool that writes one finds the
// same folder.
func TestUsersDirIsWhereTheServerReadsUsers(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if got, want := UsersDir(state), layout(state).usersDir; got != want {
		t.Errorf("UsersDir = %q, want %q", got, want)
	}
	if got, want := UsersDir("/state"), filepath.Join("/state", "shared", "users"); got != want {
		t.Errorf("UsersDir = %q, want %q", got, want)
	}
}
