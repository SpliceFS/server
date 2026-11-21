package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/internal/cli"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// userFileMode is the mode of a user file: the owner writes it, the group of the folder, which
// the server runs in, reads it, and nobody else sees a password hash.
const userFileMode os.FileMode = 0o640

var (
	errUserAddName = errors.New("user add takes one name")
	errUserExists  = errors.New("already exists")
)

// userAdd is the user add command, with the parts of the system it uses as fields so that a
// test can replace them.
type userAdd struct {
	stateTool

	// hash reads a password from its first argument and returns its hash.
	hash func(in, random io.Reader) (string, error)
}

// systemUserAdd returns user add as it runs on the machine: the state tool of the machine and
// the hash that hash-password makes.
func systemUserAdd() userAdd {
	return userAdd{stateTool: systemStateTool(), hash: passwordHash}
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
		if err := auth.CheckShare(share); err != nil {
			return err //nolint:wrapcheck // the flag package names the flag that failed
		}
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

// add makes the user file and returns the id of the user. Everything that can be refused is
// refused before the password is read, so a typo in the name costs no typing.
func (c userAdd) add(opts userAddOptions, stdin io.Reader) (string, error) {
	if err := auth.CheckName(opts.name); err != nil {
		return "", err //nolint:wrapcheck // printed to the user as it is
	}
	folder, err := c.readyFolder(opts.state, "users", app.UsersDir)
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

// write makes the file of user under a new id and returns the id.
func (c userAdd) write(folder ownedFolder, user auth.User) (string, error) {
	data, err := auth.MarshalUser(user)
	if err != nil {
		return "", err //nolint:wrapcheck // printed to the user as it is
	}
	id, err := uuid7.New(c.now(), c.random)
	if err != nil {
		return "", fmt.Errorf("make the user id: %w", err)
	}
	file := newFile{what: "user file", name: datadir.IDFile(id), data: data, mode: userFileMode}
	if err := c.writeOnce(folder, file); err != nil {
		return "", err
	}
	return id, nil
}
