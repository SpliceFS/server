package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/internal/cli"
)

var errHashPasswordArguments = errors.New("hash-password takes no arguments")

// runHashPassword implements the hash-password command.
func runHashPassword(job call) int {
	if len(job.args) > 0 {
		return job.self.UsageError(job.stderr, errHashPasswordArguments)
	}
	if err := hashPassword(job.stdin, job.stdout, rand.Reader); err != nil {
		return job.self.Fail(job.stderr, err)
	}
	return cli.ExitOK
}

// readPassword reads one line from in and returns it without its line ending. A password
// may begin and end with spaces, so nothing else is trimmed.
func readPassword(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// hashPassword reads the password from in and writes its argon2id hash to out. The salt
// comes from random.
func hashPassword(in io.Reader, out io.Writer, random io.Reader) error {
	password, err := readPassword(in)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password, random)
	if err != nil {
		// The message already says what failed and is printed to the user as it is.
		return err //nolint:wrapcheck // printed to the user as it is
	}
	if _, err := fmt.Fprintln(out, hash); err != nil {
		return fmt.Errorf("write hash: %w", err)
	}
	return nil
}
