package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/datadir"
)

var (
	errNotAFolder    = errors.New("is not a folder")
	errNoFolderOwner = errors.New("cannot read the owner of the folder")
)

// stateTool is what a command that writes into a folder of the state folder needs of the
// machine, as fields so that a test can replace them.
type stateTool struct {
	// defaultState finds the state folder when the command line names none.
	defaultState func() (string, error)
	now          func() time.Time
	random       io.Reader
	chown        func(path string, uid, gid int) error
}

// systemStateTool returns the state tool as it runs on the machine: the default state folder of
// the build, the clock and the system's randomness.
func systemStateTool() stateTool {
	return stateTool{defaultState: config.DefaultStateDir, now: time.Now, random: rand.Reader, chown: os.Chown}
}

// owner is the user and the group that own a file.
type owner struct {
	uid, gid int
}

// ownedFolder is a folder of the state folder and who owns it.
type ownedFolder struct {
	owner

	// what names the folder in the errors, such as "users".
	what string
	path string
}

// stateFolder returns the state folder that the command line names, or the default one, as the
// server does.
func (t stateTool) stateFolder(given string) (string, error) {
	return config.GivenOrDefault(given, t.defaultState) //nolint:wrapcheck // printed to the user as it is
}

// findFolder finds the folder that pick names inside the state folder, and reads who owns it.
func (t stateTool) findFolder(given, what string, pick func(stateDir string) string) (ownedFolder, error) {
	stateDir, err := t.stateFolder(given)
	if err != nil {
		return ownedFolder{}, err
	}
	path := pick(stateDir)
	info, err := os.Stat(path)
	if err != nil {
		return ownedFolder{}, fmt.Errorf("%s folder: %w", what, err)
	}
	if !info.IsDir() {
		return ownedFolder{}, fmt.Errorf("%s folder %s %w", what, path, errNotAFolder)
	}
	who, err := ownerOf(info)
	return ownedFolder{owner: who, what: what, path: path}, err
}

// newFile is a file that a command writes into a folder.
type newFile struct {
	// what names the file in the errors, such as "user file".
	what string
	name string
	data []byte
	mode os.FileMode
}

// writeOnce writes the file into the folder, owned by the owner of the folder, and never
// replaces a file that is there: then the error is fs.ErrExist. A file that cannot be given to
// that owner is removed: left behind, it would be one the server cannot read.
func (t stateTool) writeOnce(folder ownedFolder, file newFile) error {
	path := filepath.Join(folder.path, file.name)
	if err := datadir.WriteOnce(path, file.data, file.mode); err != nil {
		return fmt.Errorf("write %s: %w", file.what, err)
	}
	if err := t.chown(path, folder.uid, folder.gid); err != nil {
		err = fmt.Errorf("give the %s to the owner of the %s folder: %w", file.what, folder.what, err)
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

// ownerOf returns the owner of the file that info describes.
func ownerOf(info fs.FileInfo) (owner, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}, errNoFolderOwner
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid)}, nil
}
