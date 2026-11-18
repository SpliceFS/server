package cli

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/SpliceFS/server/internal/version"
)

var errTerminalGone = errors.New("terminal gone")

func testProgram() Program {
	return Program{Name: "tool", Usage: "usage: tool [--flag]\n"}
}

func TestHandled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		parseErr    error
		wantVersion bool
		wantStatus  int
		wantHandled bool
		wantStdout  string
		wantStderr  string
	}{
		{"nothing to answer", nil, false, 0, false, "", ""},
		{"help", flag.ErrHelp, false, ExitOK, true, "usage: tool [--flag]\n", ""},
		{
			"bad command line", errTerminalGone, false, ExitUsage, true,
			"", "tool: terminal gone\nusage: tool [--flag]\n",
		},
		{"version", nil, true, ExitOK, true, "tool " + version.String() + "\n", ""},
		{
			"a bad command line wins over the version", errTerminalGone, true, ExitUsage, true,
			"", "tool: terminal gone\nusage: tool [--flag]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			status, handled := testProgram().Handled(&stdout, &stderr, tt.parseErr, tt.wantVersion)
			if status != tt.wantStatus || handled != tt.wantHandled ||
				stdout.String() != tt.wantStdout || stderr.String() != tt.wantStderr {
				t.Errorf("got status %d handled %v stdout %q stderr %q", status, handled, stdout.String(), stderr.String())
			}
		})
	}
}

func TestUsageError(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	status := testProgram().UsageError(&stderr, errTerminalGone)
	if want := "tool: terminal gone\nusage: tool [--flag]\n"; status != ExitUsage || stderr.String() != want {
		t.Errorf("got status %d stderr %q, want %d and %q", status, stderr.String(), ExitUsage, want)
	}
}

func TestFail(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	status := testProgram().Fail(&stderr, errTerminalGone)
	if want := "tool: terminal gone\n"; status != ExitError || stderr.String() != want {
		t.Errorf("got status %d stderr %q, want %d and %q", status, stderr.String(), ExitError, want)
	}
}

func TestFlagSet(t *testing.T) {
	t.Parallel()
	fs := testProgram().FlagSet()
	fs.Bool("flag", false, "")
	if fs.Name() != "tool" {
		t.Errorf("name = %q, want tool", fs.Name())
	}
	// The flag package would print its own message and usage. The program says it instead.
	if fs.Output() != io.Discard {
		t.Error("the flag set prints on its own")
	}
	if err := fs.Parse([]string{"--bogus"}); err == nil ||
		!strings.Contains(err.Error(), "flag provided but not defined: -bogus") {
		t.Errorf("err = %v", err)
	}
	if err := fs.Parse([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h gave %v, want flag.ErrHelp", err)
	}
}
