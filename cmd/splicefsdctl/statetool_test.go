package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/SpliceFS/server/app"
)

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

// chownCall is one call of chown.
type chownCall struct {
	path     string
	uid, gid int
}

// chownLog is a chown that writes down what it is asked and fails when err is set.
type chownLog struct {
	calls []chownCall
	err   error
}

func (l *chownLog) chown(path string, uid, gid int) error {
	l.calls = append(l.calls, chownCall{path, uid, gid})
	return l.err
}

// paths returns the paths of the calls, in the order they were made.
func (l *chownLog) paths() []string {
	paths := make([]string, 0, len(l.calls))
	for _, call := range l.calls {
		paths = append(paths, call.path)
	}
	return paths
}

// checkGivenTo fails the test when a call gave a folder to anyone but the owner.
func (l *chownLog) checkGivenTo(t *testing.T, want *syscall.Stat_t) {
	t.Helper()
	for _, call := range l.calls {
		if call.uid != int(want.Uid) || call.gid != int(want.Gid) {
			t.Errorf("%s given to %d:%d, want the owner of the state folder %d:%d",
				call.path, call.uid, call.gid, want.Uid, want.Gid)
		}
	}
}

// sortedPaths returns the paths of the calls in name order, which is the order foldersBelow
// lists them in; the order of the calls is the order the server makes the folders in.
func (l *chownLog) sortedPaths() []string {
	return slices.Sorted(slices.Values(l.paths()))
}

// recordingTool is a state tool with the real folders and a chown that does nothing but write
// down what it is asked.
func recordingTool() (stateTool, *chownLog) {
	log := &chownLog{}
	tool := testStateTool(strings.NewReader(""))
	tool.chown = log.chown
	return tool, log
}

func TestReadyFolderGivesTheFoldersItMadeToTheOwnerOfTheStateFolder(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	tool, log := recordingTool()

	got, err := tool.readyFolder(state, "cluster", app.ClusterDir)

	if err != nil || got.path != app.ClusterDir(state) || got.what != "cluster" {
		t.Fatalf("readyFolder = %+v, %v", got, err)
	}
	want := foldersBelow(t, state)
	if len(want) == 0 || !reflect.DeepEqual(log.sortedPaths(), want) {
		t.Fatalf("given away: %q, want the folders made: %q", log.sortedPaths(), want)
	}
	log.checkGivenTo(t, statOf(t, state))
	// A second call finds every folder there and gives nothing away.
	log.calls = nil
	if _, err := tool.readyFolder(state, "cluster", app.ClusterDir); err != nil || len(log.calls) != 0 {
		t.Errorf("second readyFolder = %v, gave away %q, want nothing", err, log.paths())
	}
}

// The state folder is one of the folders that a server makes, so a missing one is made too and
// given to its own owner, which leaves it as it is.
func TestReadyFolderMakesAMissingStateFolder(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	tool, log := recordingTool()

	if _, err := tool.readyFolder(state, "users", app.UsersDir); err != nil {
		t.Fatal(err)
	}

	if want := append([]string{state}, foldersBelow(t, state)...); !reflect.DeepEqual(log.sortedPaths(), want) {
		t.Errorf("given away: %q, want %q", log.sortedPaths(), want)
	}
}

// A folder that cannot be given to the owner would be one the server cannot enter, so the
// folders made are removed and init can be run again.
func TestReadyFolderRemovesTheFoldersItCouldNotGiveAway(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	tool, log := recordingTool()
	log.err = errTestChown

	_, err := tool.readyFolder(state, "cluster", app.ClusterDir)

	first := filepath.Join(state, "local")
	if err == nil || !errors.Is(err, errTestChown) ||
		!strings.Contains(err.Error(), "give "+first+" to the owner of the state folder") {
		t.Errorf("readyFolder error = %v", err)
	}
	if left := fileNames(t, state); len(left) != 0 {
		t.Errorf("left in the state folder: %q", left)
	}
}

// What the server made before it met a file where a folder belongs is given away all the same:
// a folder of the root user in the state of another user would stop the server.
func TestReadyFolderGivesAwayWhatWasMadeBeforeAFailure(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "shared"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tool, log := recordingTool()

	_, err := tool.readyFolder(state, "cluster", app.ClusterDir)

	if err == nil || !strings.Contains(err.Error(), "exists and is not a folder") {
		t.Errorf("readyFolder error = %v", err)
	}
	if want := []string{filepath.Join(state, "local")}; !reflect.DeepEqual(log.paths(), want) {
		t.Errorf("given away: %q, want %q", log.paths(), want)
	}
}

// Folders that were reported as made but are not there, or a state folder that is gone, are
// reported and not guessed at.
func TestReadyFolderReportsAStateFolderItCannotFind(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "gone")
	tool, log := recordingTool()
	tool.makeFolders = func(string) ([]string, error) { return []string{state}, nil }

	_, err := tool.readyFolder(state, "cluster", app.ClusterDir)

	if err == nil || !strings.Contains(err.Error(), "state folder: ") || len(log.calls) != 0 {
		t.Errorf("readyFolder error = %v, gave away %q", err, log.paths())
	}
}

func TestReadyFolderReportsAFolderThatIsStillMissing(t *testing.T) {
	t.Parallel()
	tool, _ := recordingTool()
	tool.makeFolders = func(string) ([]string, error) { return nil, nil }

	_, err := tool.readyFolder(t.TempDir(), "cluster", app.ClusterDir)

	if err == nil || !strings.Contains(err.Error(), "cluster folder: ") {
		t.Errorf("readyFolder error = %v", err)
	}
}
