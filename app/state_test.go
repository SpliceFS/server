package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestMakeFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
		want  string // part of the expected error; empty if the call must succeed
		mode  os.FileMode
	}{
		{
			name: "missing folder is made with exactly its mode",
			mode: 0o750,
		},
		{
			name: "existing folder keeps its mode",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			mode: 0o700,
		},
		{
			name: "link to a folder is accepted",
			setup: func(t *testing.T, path string) {
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
		{
			name: "file where the folder belongs",
			setup: func(t *testing.T, path string) {
				write(t, filepath.Dir(path), filepath.Base(path), "a file")
			},
			want: "is not a folder",
		},
		{
			name: "link to nothing where the folder belongs",
			setup: func(t *testing.T, path string) {
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
			path := filepath.Join(t.TempDir(), "folder")
			if tt.setup != nil {
				tt.setup(t, path)
			}
			err := makeFolder(folder{path, 0o750})
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) ||
					!strings.Contains(err.Error(), path) {
					t.Fatalf("makeFolder = %v, want an error naming %s and containing %q",
						err, path, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() || info.Mode().Perm() != tt.mode {
				t.Errorf("folder = %v, %v, want a folder with mode %v", info, err, tt.mode)
			}
		})
	}
}

func TestNodeIDErrors(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	tests := []struct {
		name   string
		setup  func(t *testing.T, d dataPaths)
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
			setup: func(t *testing.T, d dataPaths) {
				if err := os.MkdirAll(d.nodeIDFile, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "read node id file",
		},
		{
			name: "node id cannot be written",
			setup: func(t *testing.T, d dataPaths) {
				if os.Geteuid() == 0 {
					t.Skip("root can write to a read-only folder")
				}
				dir := filepath.Dir(d.nodeIDFile)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				// Nothing can be created inside a folder that is read-only. The folder is
				// made writable again so that the test directory can be removed.
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			},
			want: "write node id file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := layout(t.TempDir())
			if tt.setup != nil {
				tt.setup(t, d)
			}
			random := tt.random
			if random == nil {
				random = zeros{}
			}
			id, err := d.nodeID(random)
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
