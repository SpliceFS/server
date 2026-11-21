package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/internal/cli"
)

// clusterKeyMode is the mode of the cluster key file: the owner, which is the user the server
// runs as, reads it, and nobody else sees the private key.
const clusterKeyMode os.FileMode = 0o600

var (
	errClusterInitArgs = errors.New("cluster init takes no arguments")
	errClusterShowArgs = errors.New("cluster show takes no arguments")
	errClusterHasKey   = errors.New("this cluster already has a key")
	errNoClusterKey    = errors.New("this cluster has no key, run " + program + " cluster init")
)

// clusterKey is the commands cluster init and cluster show, with the parts of the system they
// use as fields so that a test can replace them.
type clusterKey struct {
	stateTool
}

// systemClusterKey returns the cluster key commands as they run on the machine.
func systemClusterKey() clusterKey {
	return clusterKey{stateTool: systemStateTool()}
}

// initCommand is the entry of cluster init in the list of commands.
func (c clusterKey) initCommand() command {
	return command{name: "cluster init", args: "[--state <folder>]", run: c.runInit}
}

// showCommand is the entry of cluster show in the list of commands.
func (c clusterKey) showCommand() command {
	return command{name: "cluster show", args: "[--state <folder>]", run: c.runShow}
}

// runInit implements the cluster init command.
func (c clusterKey) runInit(job call) int {
	return serveLine(job, errClusterInitArgs, c.makeKey)
}

// runShow implements the cluster show command.
func (c clusterKey) runShow(job call) int {
	return serveLine(job, errClusterShowArgs, c.readKey)
}

// serveLine runs a cluster command: one that takes only --state, finds a key in the state folder
// it names and prints the line that a client needs. Both commands print that same line.
func serveLine(job call, noArgs error, find func(state string) (cluster.Key, error)) int {
	opts, status, done := parseStateOnly(job, noArgs)
	if done {
		return status
	}
	found, err := find(opts.state)
	if err == nil {
		_, err = fmt.Fprint(job.stdout, clientLine(found))
		if err != nil {
			err = fmt.Errorf("write the line: %w", err)
		}
	}
	if err != nil {
		return job.self.Fail(job.stderr, err)
	}
	return cli.ExitOK
}

// stateOptions are what the command line of a command that takes only --state says.
type stateOptions struct {
	state string
}

// parseStateOnly reads the command line of a command that takes only --state. It reports
// whether the command is already answered, as for --help or a bad command line, with the exit
// status. noArgs is the error of a word that is left over.
func parseStateOnly(job call, noArgs error) (stateOptions, int, bool) {
	var opts stateOptions
	flags := job.self.FlagSet()
	flags.StringVar(&opts.state, "state", "", "")
	err := flags.Parse(job.args)
	if status, handled := job.self.Handled(job.stdout, job.stderr, err, false); handled {
		return opts, status, true
	}
	if flags.NArg() != 0 {
		return opts, job.self.UsageError(job.stderr, noArgs), true
	}
	return opts, cli.ExitOK, false
}

// clientLine is the line a client puts in its configuration to trust the cluster of the key.
func clientLine(key cluster.Key) string {
	return "cluster_key: " + key.PublicText() + "\n"
}

// makeKey makes the key of the cluster and returns it. A cluster has one key for good: when the
// file is there, whatever it holds, it stays as it is.
func (c clusterKey) makeKey(given string) (cluster.Key, error) {
	folder, err := c.findFolder(given, "cluster", app.ClusterDir)
	if err != nil {
		return cluster.Key{}, err
	}
	made, err := cluster.NewKey(c.now(), c.random)
	if err != nil {
		return cluster.Key{}, err //nolint:wrapcheck // printed to the user as it is
	}
	file := newFile{
		what: "cluster key file", name: cluster.KeyFile, data: cluster.MarshalKey(made), mode: clusterKeyMode,
	}
	err = c.writeOnce(folder, file)
	if errors.Is(err, fs.ErrExist) {
		return cluster.Key{}, errClusterHasKey
	}
	if err != nil {
		return cluster.Key{}, err
	}
	return made, nil
}

// readKey returns the key of the cluster, which the file in the state folder holds. It reads the
// file with the same code as the server does, so a file that is refused here is one the server
// would not use.
func (c clusterKey) readKey(given string) (cluster.Key, error) {
	stateDir, err := c.stateFolder(given)
	if err != nil {
		return cluster.Key{}, err
	}
	path := filepath.Join(app.ClusterDir(stateDir), cluster.KeyFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cluster.Key{}, errNoClusterKey
	}
	if err != nil {
		return cluster.Key{}, fmt.Errorf("read the cluster key file: %w", err)
	}
	found, err := cluster.ParseKey(data)
	if err != nil {
		return cluster.Key{}, fmt.Errorf("cluster key file %s %w", path, err)
	}
	return found, nil
}
