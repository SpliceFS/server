// Package cli holds what the programs of the SpliceFS server say and return in the same way:
// the exit statuses, the answers to help and version requests, and the report of a bad
// command line or a failure.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/SpliceFS/server/internal/version"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Program is the name and the usage text of one program, which is what it prints about
// itself.
type Program struct {
	Name  string
	Usage string
}

// FlagSet returns a flag set that prints nothing. The program reports a bad command line
// itself (see Handled), so the text the user sees is the same for every program.
func (p Program) FlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet(p.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// Handled answers what every program answers before it does its own work, and reports
// whether it did. parseErr is the result of parsing the command line and wantVersion is the
// value of its version flag. It returns the exit status and true when the program is done,
// and false when the program has its own work to do.
func (p Program) Handled(stdout, stderr io.Writer, parseErr error, wantVersion bool) (int, bool) {
	switch {
	case errors.Is(parseErr, flag.ErrHelp):
		sayf(stdout, "%s", p.Usage)
		return ExitOK, true
	case parseErr != nil:
		return p.UsageError(stderr, parseErr), true
	case wantVersion:
		sayf(stdout, "%s %s\n", p.Name, version.String())
		return ExitOK, true
	}
	return 0, false
}

// UsageError prints err and the usage text, and returns the exit status for a bad command
// line. The text of err is what the user reads after the program name.
func (p Program) UsageError(stderr io.Writer, err error) int {
	sayf(stderr, "%s: %v\n%s", p.Name, err, p.Usage)
	return ExitUsage
}

// Fail prints err and returns the exit status for a runtime error.
func (p Program) Fail(stderr io.Writer, err error) int {
	sayf(stderr, "%s: %v\n", p.Name, err)
	return ExitError
}

// sayf writes a message to w. A failed write is ignored: it means the terminal or pipe is
// gone, so nobody is left to read a report of it, and the exit status says what happened.
func sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // nobody is left to report a failed write to
}
