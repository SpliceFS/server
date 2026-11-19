package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/SpliceFS/common/wire"
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

// The digest folder holds what this node alone may read: it is made private, like local/.
func TestDigestFolderIsPrivate(t *testing.T) {
	t.Parallel()
	paths := layout(t.TempDir())
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.digestsDir)
	if err != nil || info.Mode() != os.ModeDir|0o700 {
		t.Errorf("digest folder = %v, %v, want a folder of mode 0700", info, err)
	}
}

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
