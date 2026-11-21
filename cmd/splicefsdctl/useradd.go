package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/cli"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// userFileMode is the mode of a user file: the owner writes it, the group of the folder, which
// the server runs in, reads it, and nobody else sees a password hash.
const userFileMode os.FileMode = 0o640

var (
	errUserAddName   = errors.New("user add takes one name")
	errUserExists    = errors.New("already exists")
	errNotAFolder    = errors.New("is not a folder")
	errNoFolderOwner = errors.New("cannot read the owner of the users folder")
)

// userAdd is the user add command, with the parts of the system it uses as fields so that a
// test can replace them.
type userAdd struct {
	// defaultState finds the state folder when the command line names none.
	defaultState func() (string, error)
	// hash reads a password from its first argument and returns its hash.
	hash   func(in, random io.Reader) (string, error)
	now    func() time.Time
	random io.Reader
	chown  func(path string, uid, gid int) error
}

// systemUserAdd returns user add as it runs on the machine: the default state folder of the
// build, the hash that hash-password makes, the clock and the system's randomness.
func systemUserAdd() userAdd {
	return userAdd{
		defaultState: config.DefaultStateDir, hash: passwordHash, now: time.Now, random: rand.Reader,
		chown: os.Chown,
	}
}

// command is the entry of user add in the list of commands.
func (c userAdd) command() command {
	return command{name: "user add", args: "[--state <folder>] [--share <share>]... <name>", run: c.run}
}

// userAddOptions are what the command line of user add says.
type userAddOptions struct {
	state  string
	shares []string
	name   string
}

// run implements the user add command.
func (c userAdd) run(job call) int {
	opts, status, done := parseUserAdd(job)
	if done {
		return status
	}
	id, err := c.add(opts, job.stdin)
	if err == nil {
		_, err = fmt.Fprintln(job.stdout, id)
		if err != nil {
			err = fmt.Errorf("write id: %w", err)
		}
	}
	if err != nil {
		return job.self.Fail(job.stderr, err)
	}
	return cli.ExitOK
}

// parseUserAdd reads the command line of user add. It reports whether the command is already
// answered, as for --help or a bad command line, with the exit status.
func parseUserAdd(job call) (userAddOptions, int, bool) {
	var opts userAddOptions
	flags := job.self.FlagSet()
	flags.StringVar(&opts.state, "state", "", "")
	flags.Func("share", "", func(share string) error {
		opts.shares = append(opts.shares, share)
		return nil
	})
	err := flags.Parse(job.args)
	if status, handled := job.self.Handled(job.stdout, job.stderr, err, false); handled {
		return opts, status, true
	}
	if flags.NArg() != 1 {
		return opts, job.self.UsageError(job.stderr, errUserAddName), true
	}
	opts.name = flags.Arg(0)
	return opts, cli.ExitOK, false
}

// owner is the user and the group that own a file.
type owner struct {
	uid, gid int
}

// usersFolder is the folder of the user files and who owns it.
type usersFolder struct {
	owner

	path string
}

// add makes the user file and returns the id of the user. Everything that can be refused is
// refused before the password is read, so a typo in the name costs no typing.
func (c userAdd) add(opts userAddOptions, stdin io.Reader) (string, error) {
	if err := auth.CheckName(opts.name); err != nil {
		return "", err //nolint:wrapcheck // printed to the user as it is
	}
	folder, err := c.findUsersFolder(opts.state)
	if err != nil {
		return "", err
	}
	// The same reader as the server's, so that what it counts as a taken name is the same.
	users := auth.NewUsers(folder.path, c.now, slog.New(slog.DiscardHandler))
	if _, taken := users.Lookup(opts.name); taken {
		return "", fmt.Errorf("user %q %w", opts.name, errUserExists)
	}
	hash, err := c.hash(stdin, c.random)
	if err != nil {
		return "", err
	}
	return c.write(folder, auth.User{Name: opts.name, PasswordHash: hash, Shares: opts.shares})
}

// findUsersFolder finds the folder of the user files in the given state folder, or the default
// one, as the server does, and reads who owns it.
func (c userAdd) findUsersFolder(state string) (usersFolder, error) {
	stateDir, err := config.GivenOrDefault(state, c.defaultState)
	if err != nil {
		return usersFolder{}, err //nolint:wrapcheck // printed to the user as it is
	}
	path := app.UsersDir(stateDir)
	info, err := os.Stat(path)
	if err != nil {
		return usersFolder{}, fmt.Errorf("users folder: %w", err)
	}
	if !info.IsDir() {
		return usersFolder{}, fmt.Errorf("users folder %s %w", path, errNotAFolder)
	}
	who, err := ownerOf(info)
	return usersFolder{path: path, owner: who}, err
}

// write makes the file of user under a new id, owned by the owner of the folder. A file that
// cannot be given to that owner is removed: left behind, it would be a user the server cannot
// read.
func (c userAdd) write(folder usersFolder, user auth.User) (string, error) {
	data, err := auth.MarshalUser(user)
	if err != nil {
		return "", err //nolint:wrapcheck // printed to the user as it is
	}
	id, err := uuid7.New(c.now(), c.random)
	if err != nil {
		return "", fmt.Errorf("make the user id: %w", err)
	}
	path := filepath.Join(folder.path, datadir.IDFile(id))
	if err := datadir.WriteOnce(path, data, userFileMode); err != nil {
		return "", fmt.Errorf("write user file: %w", err)
	}
	if err := c.chown(path, folder.uid, folder.gid); err != nil {
		err = fmt.Errorf("give the user file to the owner of the users folder: %w", err)
		return "", errors.Join(err, os.Remove(path))
	}
	return id, nil
}

// ownerOf returns the owner of the file that info describes.
func ownerOf(info fs.FileInfo) (owner, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}, errNoFolderOwner
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid)}, nil
}
