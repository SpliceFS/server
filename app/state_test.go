package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

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
			want:   "make node id: injected",
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
			id, err := paths.nodeID(random)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("nodeID = %q, %v, want an error containing %q", id, err, tt.want)
			}
		})
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
