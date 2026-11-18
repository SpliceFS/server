package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/internal/cli"
)

// argon2idPrefix starts every hash that hash-password makes, with the parameters it uses.
const argon2idPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"

// checkHashOutput fails the test when out is not one line with the hash of want.
func checkHashOutput(t *testing.T, out, want string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, argon2idPrefix) {
		t.Fatalf("output = %q", out)
	}
	hash := strings.TrimSuffix(out, "\n")
	if ok, err := auth.VerifyPassword(hash, want); err != nil || !ok {
		t.Errorf("the hash does not verify %q: %v, %v", want, ok, err)
	}
}

func TestReadPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"line", "correct horse\n", "correct horse"},
		{"no line ending", "correct horse", "correct horse"},
		{"windows line ending", "correct horse\r\n", "correct horse"},
		{"only the first line", "first\nsecond\n", "first"},
		{"spaces are kept", "  pad  \n", "  pad  "},
		{"empty input", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readPassword(strings.NewReader(tt.input))
			if err != nil || got != tt.want {
				t.Errorf("readPassword(%q) = %q, %v, want %q", tt.input, got, err, tt.want)
			}
		})
	}
}

// The hash is made with the real cost, so one run through the whole program is enough: what
// the program takes as the password is proven by TestReadPassword.
func TestHashPasswordCommand(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	status := run([]string{"hash-password"}, strings.NewReader("correct horse\n"), &out, &errOut)
	if status != cli.ExitOK || errOut.Len() != 0 {
		t.Fatalf("status %d stderr %q", status, errOut.String())
	}
	checkHashOutput(t, out.String(), "correct horse")
}

func TestHashPasswordErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     io.Reader
		out    io.Writer
		random io.Reader
		want   string
	}{
		{"empty input", strings.NewReader(""), io.Discard, saltSource(), "empty password"},
		{"empty line", strings.NewReader("\n"), io.Discard, saltSource(), "empty password"},
		{
			"unreadable input", iotest.ErrReader(errTerminalGone), io.Discard, saltSource(),
			"read password: terminal gone",
		},
		{"no randomness", strings.NewReader("pw\n"), io.Discard, strings.NewReader("short"), "read salt"},
		{"unwritable output", strings.NewReader("pw\n"), errWriter{}, saltSource(), "write hash: disk full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := hashPassword(tt.in, tt.out, tt.random)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// The failures that the tests make the program's input and output report.
var (
	errTerminalGone = errors.New("terminal gone")
	errDiskFull     = errors.New("disk full")
)

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errDiskFull }

// saltSource returns a source of random bytes that is long enough for one salt.
func saltSource() io.Reader { return strings.NewReader("0123456789abcdef") }

func TestHashPasswordCommandFailure(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	status := run([]string{"hash-password"}, strings.NewReader("\n"), &out, &errOut)
	if status != cli.ExitError || out.Len() != 0 || errOut.String() != "splicefsdctl: hash password: empty password\n" {
		t.Errorf("status %d stdout %q stderr %q", status, out.String(), errOut.String())
	}
}
