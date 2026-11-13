// Command splicefsd is the SpliceFS server. It serves the shares of its configuration
// directory over HTTP. The config subcommands check and show that configuration, and
// hash-password makes a password hash for a user file.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/SpliceFS/server/app"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/version"
)

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const (
	program = "splicefsd"
	usage   = `usage: splicefsd [--config DIR] [--state DIR]
       splicefsd [--config DIR] config check
       splicefsd [--config DIR] config show
       splicefsd hash-password
       splicefsd --version

SIGHUP applies the configuration without a restart.
`
)

// exit is replaced in tests, which is the only way to run main inside the test process
// without ending it.
var exit = os.Exit //nolint:gochecknoglobals // the one seam that lets a test run main

// The errors of a bad command line. They are printed after the program name, so their text
// is what the user reads. A message that follows a quoted value holds only the words after
// that value.
var (
	errConfigCommand      = errors.New("config needs one of: check, show")
	errUnexpectedArgument = errors.New("unexpected argument")
)

func main() {
	exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. It returns the exit status. The server stops when ctx is
// cancelled or the process receives SIGINT or SIGTERM.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "hash-password" {
		return runHashPassword(args[1:], stdin, stdout, stderr)
	}
	opts, err := parseArgs(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		sayf(stdout, "%s", usage)
		return exitOK
	case err != nil:
		sayf(stderr, "%s: %v\n%s", program, err, usage)
		return exitUsage
	case opts.showVersion:
		sayf(stdout, "%s %s\n", program, version.String())
		return exitOK
	}
	if opts.command != "" {
		return runConfig(opts, systemDirs(), stdout, stderr)
	}
	return runServer(ctx, opts, systemDirs(), stderr)
}

// The subcommands after "config".
const (
	commandCheck = "check"
	commandShow  = "show"
)

// options are the settings of the command line. An empty directory means the default.
type options struct {
	configDir   string
	stateDir    string
	showVersion bool
	// command is check or show for the config subcommands, empty to run the server.
	command string
}

// parseArgs reads the command line of the server.
func parseArgs(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet(program, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.configDir, "config", "", "")
	fs.StringVar(&opts.stateDir, "state", "", "")
	fs.BoolVar(&opts.showVersion, "version", false, "")
	if err := fs.Parse(args); err != nil {
		// The text of the flag package's error is what the user is shown, so it gets no prefix.
		return options{}, err //nolint:wrapcheck // printed to the user as it is
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
	case rest[0] == "config" && len(rest) == 2 && isConfigCommand(rest[1]):
		opts.command = rest[1]
	case rest[0] == "config":
		return options{}, errConfigCommand
	default:
		return options{}, fmt.Errorf("%w %q", errUnexpectedArgument, rest[0])
	}
	return opts, nil
}

// defaultDirs finds the directories used when the command line names none. They are looked
// up only when needed, because the home directory they may be under is not always known.
type defaultDirs struct {
	config, state func() (string, error)
}

// systemDirs returns the defaults of this build.
func systemDirs() defaultDirs {
	return defaultDirs{config: config.DefaultConfigDir, state: config.DefaultStateDir}
}

func isConfigCommand(name string) bool { return name == commandCheck || name == commandShow }

// directory returns given, or the default when none was given.
func directory(given string, fallback func() (string, error)) (string, error) {
	if given != "" {
		return given, nil
	}
	return fallback()
}

// runConfig implements config check and config show.
func runConfig(opts options, defaults defaultDirs, stdout, stderr io.Writer) int {
	dir, err := directory(opts.configDir, defaults.config)
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.LoadDir(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if opts.command == commandCheck {
		_, err = io.WriteString(stdout, "configuration ok\n")
	} else {
		err = showConfig(stdout, cfg)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// runHashPassword implements the hash-password subcommand.
func runHashPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		sayf(stderr, "%s: hash-password takes no arguments\n%s", program, usage)
		return exitUsage
	}
	if err := hashPassword(stdin, stdout, rand.Reader); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// hashPassword reads one line from in and writes the argon2id hash of it to out. The salt
// comes from random. The password is the line without its line ending.
func hashPassword(in io.Reader, out io.Writer, random io.Reader) error {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
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

// runServer loads the configuration and serves until ctx is cancelled or a termination
// signal arrives. An invalid configuration is reported before anything is opened. SIGHUP
// applies the configuration again.
func runServer(ctx context.Context, opts options, defaults defaultDirs, stderr io.Writer) int {
	configDir, err := directory(opts.configDir, defaults.config)
	if err != nil {
		return fail(stderr, err)
	}
	stateDir, err := directory(opts.stateDir, defaults.state)
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.LoadDir(configDir)
	if err != nil {
		return fail(stderr, err)
	}
	var level slog.LevelVar
	level.Set(levelOf(cfg.Log))
	logger := newLogger(cfg.Log.Format, &level, stderr)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The signal package never blocks on a full channel, so the channel needs room for one
	// signal. A second SIGHUP that arrives while one is waiting is dropped, which loses
	// nothing: the reload that is waiting reads the files after both changes.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
	err = app.Run(ctx, cfg, stateDir, app.Options{
		Version: version.String(),
		Logger:  logger,
		Ready:   func(addrs []net.Addr) { logListening(logger, addrs) },
		Reload:  reload,
		Load:    func() (*config.Config, error) { return config.LoadDir(configDir) },
		Applied: followLevel(&level),
	})
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// logListening tells the operator where the server is reachable.
func logListening(logger *slog.Logger, addrs []net.Addr) {
	for _, addr := range addrs {
		logger.Info("listening", "address", addr.String())
	}
}

// levelOf returns the log level the settings name. The configuration loader has already
// rejected unknown names.
func levelOf(cfg config.Log) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return slog.LevelInfo
	}
	return level
}

// followLevel returns the function that gives a reloaded configuration's log level to level.
func followLevel(level *slog.LevelVar) func(*config.Config) {
	return func(cfg *config.Config) { level.Set(levelOf(cfg.Log)) }
}

// newLogger returns a logger that writes to w in the given format, json or text, at the
// level in level. The format is fixed here, because a handler is built once and a change of
// format in the configuration needs a restart. The level is read for every record, so a
// reload can change it. The configuration loader has already rejected unknown formats.
func newLogger(format string, level *slog.LevelVar, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// fail prints err and returns the exit status for a runtime error.
func fail(stderr io.Writer, err error) int {
	sayf(stderr, "%s: %v\n", program, err)
	return exitError
}

// sayf writes a message to w. A failed write is ignored: it means the terminal or pipe is
// gone, so nobody is left to read a report of it, and the exit status says what happened.
func sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // nobody is left to report a failed write to
}
