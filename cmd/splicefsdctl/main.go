// Command splicefsdctl is the control tool of the SpliceFS server. It does the jobs an
// administrator runs by hand, such as making a password hash for a user file, and leaves
// the serving to splicefsd.
//
// A command is one entry in commands: its name, the arguments its usage line shows, and the
// function that runs it. The usage text and the dispatch both come from that list.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/SpliceFS/server/internal/cli"
)

const program = "splicefsdctl"

// exit is replaced in tests, which is the only way to run main inside the test process
// without ending it.
var exit = os.Exit //nolint:gochecknoglobals // the one seam that lets a test run main

// The errors of a bad command line. They are printed after the program name, so their text
// is what the user reads. A message that follows a quoted value holds only the words after
// that value.
var (
	errNoCommand      = errors.New("a command is needed, one of")
	errUnknownCommand = errors.New("unknown command")
)

// call is what a command runs with: how the program speaks, the arguments after the name of
// the command, and the program's streams.
type call struct {
	self           cli.Program
	args           []string
	stdin          io.Reader
	stdout, stderr io.Writer
}

// command is one thing the tool does.
type command struct {
	// name is the words that start the command line, such as "hash-password" or "user add".
	name string
	// args is what the usage line shows after the name, empty when there is none.
	args string
	run  func(call) int
}

// commands lists every command of the tool. A new command is one more entry here.
func commands() []command {
	key := systemClusterKey()
	return []command{
		{name: "hash-password", run: runHashPassword},
		systemUserAdd().command(),
		key.initCommand(),
		key.showCommand(),
	}
}

func main() {
	exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. It returns the exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runCommands(commands(), args, stdin, stdout, stderr)
}

// runCommands is run for the given commands.
func runCommands(cmds []command, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	self := cli.Program{Name: program, Usage: usageText(cmds)}
	var showVersion bool
	fs := self.FlagSet()
	fs.BoolVar(&showVersion, "version", false, "")
	err := fs.Parse(args)
	if status, handled := self.Handled(stdout, stderr, err, showVersion); handled {
		return status
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return self.UsageError(stderr, fmt.Errorf("%w: %s", errNoCommand, strings.Join(names(cmds), ", ")))
	}
	cmd, cmdArgs, found := find(cmds, rest)
	if !found {
		return self.UsageError(stderr, fmt.Errorf("%w %q", errUnknownCommand, rest[0]))
	}
	return cmd.run(call{self: self, args: cmdArgs, stdin: stdin, stdout: stdout, stderr: stderr})
}

// usageText is the usage of the tool: one line per command, then the version flag.
func usageText(cmds []command) string {
	lines := make([]string, 0, len(cmds)+1)
	for _, cmd := range cmds {
		lines = append(lines, strings.TrimSpace(program+" "+cmd.name+" "+cmd.args))
	}
	lines = append(lines, program+" --version")
	return "usage: " + strings.Join(lines, "\n       ") + "\n"
}

func names(cmds []command) []string {
	list := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		list = append(list, cmd.name)
	}
	return list
}

// find returns the command whose words start the command line after the flags, and what
// follows those words.
func find(cmds []command, words []string) (command, []string, bool) {
	for _, cmd := range cmds {
		own := strings.Fields(cmd.name)
		if len(words) >= len(own) && slices.Equal(words[:len(own)], own) {
			return cmd, words[len(own):], true
		}
	}
	return command{}, nil, false
}
