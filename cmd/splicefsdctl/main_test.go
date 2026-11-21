package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SpliceFS/server/internal/cli"
	"github.com/SpliceFS/server/internal/version"
)

//nolint:paralleltest // replaces the package variable exit and os.Args for the whole process
func TestMainRunsTheProgram(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	status := -1
	exit = func(code int) { status = code }
	os.Args = []string{"splicefsdctl", "--version"}

	main()

	if status != cli.ExitOK {
		t.Errorf("exit status = %d, want %d", status, cli.ExitOK)
	}
}

// outcome is what one run of the program did: its exit status and what it printed.
type outcome struct {
	status         int
	stdout, stderr string
}

// execute runs the program with no input.
func execute(t *testing.T, args ...string) outcome {
	t.Helper()
	return executeWith(t, commands(), "", args...)
}

// executeWith runs the program with the given commands and standard input.
func executeWith(t *testing.T, cmds []command, input string, args ...string) outcome {
	t.Helper()
	var out, errOut bytes.Buffer
	status := runCommands(cmds, args, strings.NewReader(input), &out, &errOut)
	return outcome{status: status, stdout: out.String(), stderr: errOut.String()}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	got := execute(t, "--version")
	want := outcome{status: cli.ExitOK, stdout: "splicefsdctl " + version.String() + "\n"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// fullUsage is the usage of the tool with its commands, which every help and every bad command
// line prints.
const fullUsage = "usage: splicefsdctl hash-password\n" +
	"       splicefsdctl user add [--state <folder>] [--share <share>]... <name>\n" +
	"       splicefsdctl --version\n"

func TestHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		if got, want := execute(t, arg), (outcome{status: cli.ExitOK, stdout: fullUsage}); got != want {
			t.Errorf("%s: got %+v, want %+v", arg, got, want)
		}
	}
}

func TestBadUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--bogus"}, "flag provided but not defined: -bogus"},
		{"no command", nil, "a command is needed, one of: hash-password, user add"},
		{"flags only", []string{"--version=false"}, "a command is needed, one of: hash-password, user add"},
		{"unknown command", []string{"serve"}, `unknown command "serve"`},
		{"a command that is only the start of one", []string{"user"}, `unknown command "user"`},
		{"a command that is not the end of one", []string{"user", "remove"}, `unknown command "user"`},
		{"command after a flag", []string{"--version=false", "serve"}, `unknown command "serve"`},
		{"command arguments", []string{"hash-password", "secret"}, "hash-password takes no arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := execute(t, tt.args...)
			if got.status != cli.ExitUsage || got.stdout != "" {
				t.Errorf("status %d stdout %q", got.status, got.stdout)
			}
			if !strings.HasPrefix(got.stderr, "splicefsdctl: ") || !strings.Contains(got.stderr, tt.want) ||
				!strings.HasSuffix(got.stderr, fullUsage) {
				t.Errorf("stderr = %q", got.stderr)
			}
		})
	}
}

// fakeCommands is a command list of its own, to prove that a command is added by one entry:
// the usage, the dispatch and the streams all come from it.
func fakeCommands() []command {
	echo := func(job call) int {
		data, err := io.ReadAll(job.stdin)
		if err != nil {
			return job.self.Fail(job.stderr, err)
		}
		_, err = io.WriteString(job.stdout, strings.Join(job.args, ",")+":"+string(data))
		if err != nil {
			return job.self.Fail(job.stderr, err)
		}
		return 7
	}
	return []command{
		{name: "first", run: func(call) int { return 3 }},
		{name: "echo", args: "[WORD ...]", run: echo},
	}
}

func TestACommandIsOneEntry(t *testing.T) {
	t.Parallel()
	cmds := fakeCommands()

	wantUsage := "usage: splicefsdctl first\n       splicefsdctl echo [WORD ...]\n       splicefsdctl --version\n"
	if got, want := executeWith(t, cmds, "", "-h"), (outcome{status: cli.ExitOK, stdout: wantUsage}); got != want {
		t.Errorf("help: got %+v, want %+v", got, want)
	}
	if got := executeWith(t, cmds, "", "first"); got.status != 3 {
		t.Errorf("first: got %+v, want status 3", got)
	}
	got := executeWith(t, cmds, "input", "echo", "a", "b")
	if want := (outcome{status: 7, stdout: "a,b:input"}); got != want {
		t.Errorf("echo: got %+v, want %+v", got, want)
	}
	got = executeWith(t, cmds, "", "none")
	if !strings.Contains(got.stderr, "unknown command \"none\"") || got.status != cli.ExitUsage {
		t.Errorf("unknown: got %+v", got)
	}
	got = executeWith(t, cmds, "")
	if !strings.Contains(got.stderr, "a command is needed, one of: first, echo") || got.status != cli.ExitUsage {
		t.Errorf("no command: got %+v", got)
	}
}

func TestAMultiWordCommandIsFoundByAllItsWords(t *testing.T) {
	t.Parallel()
	cmds := []command{
		{name: "group item", args: "NAME", run: func(job call) int {
			_, err := io.WriteString(job.stdout, strings.Join(job.args, ","))
			if err != nil {
				return job.self.Fail(job.stderr, err)
			}
			return 0
		}},
	}
	if got, want := executeWith(t, cmds, "", "group", "item", "x", "y"), (outcome{stdout: "x,y"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, args := range [][]string{{"group"}, {"group", "other"}, {"item"}} {
		got := executeWith(t, cmds, "", args...)
		if got.status != cli.ExitUsage || !strings.Contains(got.stderr, "unknown command "+`"`+args[0]+`"`) {
			t.Errorf("%q: got %+v", args, got)
		}
	}
}
