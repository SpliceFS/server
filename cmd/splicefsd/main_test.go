package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/version"
	"golang.org/x/crypto/argon2"
)

//nolint:paralleltest // replaces the package variable exit and os.Args for the whole process
func TestMainRunsTheProgram(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	status := -1
	exit = func(code int) { status = code }
	os.Args = []string{"splicefsd", "--version"}

	main()

	if status != exitOK {
		t.Errorf("exit status = %d, want %d", status, exitOK)
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
	var out, errOut bytes.Buffer
	status := run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return outcome{status: status, stdout: out.String(), stderr: errOut.String()}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	got := execute(t, "--version")
	want := outcome{status: exitOK, stdout: "splicefsd " + version.String() + "\n"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		if got, want := execute(t, arg), (outcome{status: exitOK, stdout: usage}); got != want {
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
		{"missing value", []string{"--config"}, "flag needs an argument"},
		{"unexpected argument", []string{"serve"}, `unexpected argument "serve"`},
		{"argument after flags", []string{"--config", "/x", "extra"}, `unexpected argument "extra"`},
		{"subcommand arguments", []string{"hash-password", "secret"}, "hash-password takes no arguments"},
		{"config without a command", []string{"config"}, "config needs one of: check, show"},
		{"unknown config command", []string{"config", "edit"}, "config needs one of: check, show"},
		{"extra config argument", []string{"config", "check", "x"}, "config needs one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := execute(t, tt.args...)
			if got.status != exitUsage || got.stdout != "" {
				t.Errorf("status %d stdout %q", got.status, got.stdout)
			}
			if !strings.HasPrefix(got.stderr, "splicefsd: ") || !strings.Contains(got.stderr, tt.want) ||
				!strings.HasSuffix(got.stderr, usage) {
				t.Errorf("stderr = %q", got.stderr)
			}
		})
	}
}

// argon2idPrefix starts every hash that hash-password makes, with the parameters it uses.
const argon2idPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"

// checkHashOutput fails the test when out is not one line with the hash of want, and not of
// any other password.
func checkHashOutput(t *testing.T, out, want string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, argon2idPrefix) {
		t.Fatalf("output = %q", out)
	}
	hash := strings.TrimSuffix(out, "\n")
	if ok, err := auth.VerifyPassword(hash, want); err != nil || !ok {
		t.Errorf("the hash does not verify %q: %v, %v", want, ok, err)
	}
	if ok, err := auth.VerifyPassword(hash, want+"x"); err != nil || ok {
		t.Errorf("the hash verifies another password: %v, %v", ok, err)
	}
}

func TestHashPassword(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			args := []string{"hash-password"}
			status := run(context.Background(), args, strings.NewReader(tt.input), &out, &errOut)
			if status != exitOK || errOut.Len() != 0 {
				t.Fatalf("status %d stderr %q", status, errOut.String())
			}
			checkHashOutput(t, out.String(), tt.want)
		})
	}
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
	status := run(context.Background(), []string{"hash-password"}, strings.NewReader("\n"), &out, &errOut)
	if status != exitError || out.Len() != 0 || errOut.String() != "splicefsd: hash password: empty password\n" {
		t.Errorf("status %d stdout %q stderr %q", status, out.String(), errOut.String())
	}
}

func TestNewLogger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cfg       config.Log
		debugSeen bool
		infoSeen  bool
		json      bool
	}{
		{"debug text", config.Log{Level: "debug", Format: "text"}, true, true, false},
		{"info json", config.Log{Level: "info", Format: "json"}, false, true, true},
		{"warn text", config.Log{Level: "warn", Format: "text"}, false, false, false},
		{"error json", config.Log{Level: "error", Format: "json"}, false, false, true},
		{"unknown names mean info and text", config.Log{Level: "loud", Format: "xml"}, false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			var level slog.LevelVar
			level.Set(levelOf(tt.cfg))
			logger := newLogger(tt.cfg.Format, &level, &buf)
			logger.Debug("debug-line")
			logger.Info("info-line")
			logger.Error("error-line")
			out := buf.String()
			if got := strings.Contains(out, "debug-line"); got != tt.debugSeen {
				t.Errorf("debug line logged = %v, want %v\n%s", got, tt.debugSeen, out)
			}
			if got := strings.Contains(out, "info-line"); got != tt.infoSeen {
				t.Errorf("info line logged = %v, want %v\n%s", got, tt.infoSeen, out)
			}
			if !strings.Contains(out, "error-line") {
				t.Errorf("error line missing\n%s", out)
			}
			if got := strings.Contains(out, `"msg":"error-line"`); got != tt.json {
				t.Errorf("JSON output = %v, want %v\n%s", got, tt.json, out)
			}
		})
	}
}

func TestFollowLevel(t *testing.T) {
	t.Parallel()
	var level slog.LevelVar
	level.Set(levelOf(config.Log{Level: "info"}))
	apply := followLevel(&level)
	for _, tt := range []struct {
		name string
		want slog.Level
	}{
		{"debug", slog.LevelDebug}, {"error", slog.LevelError}, {"info", slog.LevelInfo},
	} {
		apply(&config.Config{Log: config.Log{Level: tt.name}})
		if got := level.Level(); got != tt.want {
			t.Errorf("after a reload to %s the level is %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLogListening(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logListening(slog.New(slog.NewTextHandler(&buf, nil)), []net.Addr{
		&net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 8480},
		&net.TCPAddr{IP: net.IPv4(192, 0, 2, 11), Port: 8481},
	})
	out := buf.String()
	if strings.Count(out, "listening") != 2 || !strings.Contains(out, "192.0.2.10:8480") ||
		!strings.Contains(out, "192.0.2.11:8481") {
		t.Errorf("log = %q", out)
	}
}

// freePort returns a loopback port that nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is a %T, want a TCP address", listener.Addr())
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr.Port
}

// writeFiles creates the files, given by their path below dir, with their content.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// lowCostHash builds a valid argon2id hash with cheap parameters, so that no test needs the
// 64 MiB of the real ones.
func lowCostHash(password string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 8, 1, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1$%s$%s",
		enc.EncodeToString(salt), enc.EncodeToString(key))
}

// serverSetup is where a test server reads its configuration and keeps its state, and the
// port it listens on.
type serverSetup struct {
	configDir, stateDir string
	port                int
}

// healthURL is the address of the health check of the server.
func (setup serverSetup) healthURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(setup.port) + "/api/v1/health"
}

// setUpServer writes a configuration directory for a server on a free loopback port, and a
// state directory that holds only what an administrator makes (a user). The server makes
// everything else on its first start, so the tests that need other files write them.
func setUpServer(t *testing.T) serverSetup {
	t.Helper()
	port := freePort(t)
	root := t.TempDir()
	configDir, stateDir := filepath.Join(root, "config"), filepath.Join(root, "state")
	writeFiles(t, configDir, map[string]string{
		"conf.d/10-server.yaml": "listen:\n  - address: 127.0.0.1\n    port: " + strconv.Itoa(port) + "\n" +
			"    plain: true\n" +
			"cluster:\n  name: example\n  node: east\nlog:\n  level: info\n  format: json\n",
		"shares.d/media.yaml": "path: " + filepath.Join(root, "media") + "\n",
	})
	writeFiles(t, filepath.Join(root, "media"), map[string]string{"hello.txt": "hello"})
	writeFiles(t, stateDir, map[string]string{
		"shared/users/alice.yaml": "password_hash: \"" + lowCostHash("secret") + "\"\n" +
			"shares: [media]\n",
	})
	return serverSetup{configDir: configDir, stateDir: stateDir, port: port}
}

// watchedLog collects what the server logs, and tells when it has started listening.
type watchedLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newWatchedLog() *watchedLog { return &watchedLog{ready: make(chan struct{})} }

func (w *watchedLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written, err := w.buf.Write(data)
	if strings.Contains(w.buf.String(), `"msg":"listening"`) {
		w.once.Do(func() { close(w.ready) })
	}
	return written, err
}

func (w *watchedLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// runningServer is a server that a test started in the background.
type runningServer struct {
	logs   *watchedLog
	status <-chan int // receives the exit status when the server has stopped
}

// startServer runs the server in the background and waits until it listens.
func startServer(ctx context.Context, t *testing.T, setup serverSetup) runningServer {
	t.Helper()
	logs := newWatchedLog()
	status := make(chan int, 1)
	go func() {
		args := []string{"--config", setup.configDir, "--state", setup.stateDir}
		status <- run(ctx, args, strings.NewReader(""), io.Discard, logs)
	}()
	select {
	case <-logs.ready:
	case code := <-status:
		t.Fatalf("the server exited with status %d: %s", code, logs)
	}
	return runningServer{logs: logs, status: status}
}

func TestServerRunsUntilCancelled(t *testing.T) {
	t.Parallel()
	setup := setUpServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := startServer(ctx, t, setup)

	body := fetch(t, setup.healthURL())
	want := `"version":"` + version.String() + `"`
	if !strings.Contains(body, want) || !strings.Contains(body, `"node":"east"`) {
		t.Errorf("health = %s", body)
	}
	cancel()
	if code := <-server.status; code != exitOK {
		t.Errorf("exit status = %d, want 0\n%s", code, server.logs)
	}
	if !strings.Contains(server.logs.String(), `"msg":"request"`) {
		t.Errorf("no access log:\n%s", server.logs)
	}
}

func TestServerFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, configDir, stateDir string)
		want   string
	}{
		{"invalid configuration", func(t *testing.T, configDir, _ string) {
			t.Helper()
			writeFiles(t, configDir, map[string]string{"conf.d/20-bad.yaml": "listen: []\n"})
		}, "invalid server config"},
		{"unreadable configuration", func(t *testing.T, configDir, _ string) {
			t.Helper()
			writeFiles(t, configDir, map[string]string{"conf.d/20-bad.yaml": "unknown: 1\n"})
		}, "read server config"},
		{"share directory is missing", func(t *testing.T, configDir, _ string) {
			t.Helper()
			missing := map[string]string{"shares.d/media.yaml": "path: /nonexistent/media\n"}
			writeFiles(t, configDir, missing)
		}, `share "media"`},
		// A server never replaces a node id or a key it cannot read: other nodes know them.
		{"node id file is malformed", func(t *testing.T, _, stateDir string) {
			t.Helper()
			writeFiles(t, stateDir, map[string]string{"local/node-id": "not an id\n"})
		}, "node id file"},
		{"signing key file is invalid", func(t *testing.T, _, stateDir string) {
			t.Helper()
			writeFiles(t, stateDir, map[string]string{
				"local/node-id":                     "0123456789abcdef\n",
				"shared/keys/0123456789abcdef.yaml": "keys: []\n",
			})
		}, "signing key file"},
		{"data folder is a file", func(t *testing.T, _, stateDir string) {
			t.Helper()
			writeFiles(t, stateDir, map[string]string{"shared/keys": "a file where a folder belongs"})
		}, "is not a folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := setUpServer(t)
			tt.mutate(t, setup.configDir, setup.stateDir)
			var out, errOut bytes.Buffer
			args := []string{"--config", setup.configDir, "--state", setup.stateDir}
			status := run(context.Background(), args, strings.NewReader(""), &out, &errOut)
			if status != exitError || out.Len() != 0 {
				t.Errorf("status %d stdout %q", status, out.String())
			}
			got := errOut.String()
			if !strings.HasPrefix(got, "splicefsd: ") || !strings.Contains(got, tt.want) ||
				!strings.HasSuffix(got, "\n") {
				t.Errorf("stderr = %q", got)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want options
	}{
		{nil, options{}},
		{
			[]string{"-config", "/c", "--state", "/s", "--version"},
			options{configDir: "/c", stateDir: "/s", showVersion: true},
		},
		{
			[]string{"--config", "/c", "config", "check"},
			options{configDir: "/c", command: commandCheck},
		},
		{[]string{"config", "show"}, options{command: commandShow}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args)
		if err != nil || got != tt.want {
			t.Errorf("parseArgs(%q) = %+v, %v, want %+v", tt.args, got, err, tt.want)
		}
	}
}

func TestDirectory(t *testing.T) {
	t.Parallel()
	lookup := func() (string, error) { return "/default", nil }
	if got, err := directory("/given", lookup); err != nil || got != "/given" {
		t.Errorf("directory = %q, %v", got, err)
	}
	if got, err := directory("", lookup); err != nil || got != "/default" {
		t.Errorf("directory = %q, %v", got, err)
	}
}

func TestDefaultsThatCannotBeFound(t *testing.T) {
	t.Parallel()
	failing := func() (string, error) { return "", errors.New("no home directory") }
	found := func() (string, error) { return t.TempDir(), nil }
	tests := []struct {
		name     string
		defaults defaultDirs
		opts     options
	}{
		{"config command", defaultDirs{config: failing}, options{command: commandCheck}},
		{"server configuration", defaultDirs{config: failing}, options{}},
		{"server state", defaultDirs{config: found, state: failing}, options{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			var status int
			if tt.opts.command != "" {
				status = runConfig(tt.opts, tt.defaults, &out, &errOut)
			} else {
				status = runServer(context.Background(), tt.opts, tt.defaults, &errOut)
			}
			const want = "splicefsd: no home directory\n"
			if status != exitError || out.Len() != 0 || errOut.String() != want {
				t.Errorf("status %d stdout %q stderr %q", status, out.String(), errOut.String())
			}
		})
	}
}

func TestConfigCheck(t *testing.T) {
	t.Parallel()
	setup := setUpServer(t)
	for _, dir := range []string{setup.configDir, filepath.Join(t.TempDir(), "empty")} {
		got := execute(t, "--config", dir, "config", "check")
		if want := (outcome{status: exitOK, stdout: "configuration ok\n"}); got != want {
			t.Errorf("%s: got %+v, want %+v", dir, got, want)
		}
	}
}

func TestConfigCheckPrintsEveryProblem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"conf.d/a.yaml":     "log:\n  level: loud\nbase_path: api\n",
		"shares.d/m.yaml":   "path: relative\n",
		"shares.d/Bad.yaml": "path: /srv\n",
	})
	got := execute(t, "--config", dir, "config", "check")
	if got.status != exitError || got.stdout != "" {
		t.Errorf("status %d stdout %q", got.status, got.stdout)
	}
	wanted := []string{"log.level", "base_path", "shares.m.path", "shares.Bad", "a.yaml", "m.yaml"}
	for _, want := range wanted {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, got.stderr)
		}
	}
}

func TestConfigShow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"conf.d/10-base.yaml": "listen:\n  - address: 127.0.0.1\n    port: 9000\n" +
			"    plain: true\ncluster:\n  node: east\ntrusted_proxies: [192.0.2.1]\n",
		"conf.d/20-more.yaml": "auth:\n  token_ttl: 2h\n",
		"shares.d/media.yaml": "path: /srv/media\nmin_age: 5m\nexclude: []\n" +
			"follow_symlinks: true\n",
		"shares.d/plain.yaml": "path: /srv/plain\n",
	})
	got := execute(t, "--config", dir, "config", "show")
	if got.status != exitOK || got.stderr != "" {
		t.Fatalf("status %d stderr %q", got.status, got.stderr)
	}
	want := `listen: # 10-base.yaml
  - address: 127.0.0.1
    port: 9000
    plain: true
base_path: /api
cluster:
  name: default
  node: east # 10-base.yaml
  url: ""
auth:
  token_ttl: 2h # 20-more.yaml
  login_rate: 10/m
mtime_precision: 1s
log:
  level: info
  format: text
trusted_proxies: # 10-base.yaml
  - 192.0.2.1/32
shares:
  media:
    path: /srv/media # media.yaml
    exclude: [] # media.yaml
    min_age: 5m # media.yaml
    follow_symlinks: true # media.yaml
  plain:
    path: /srv/plain # plain.yaml
`
	if got.stdout != want {
		t.Errorf("output:\n%s\nwant:\n%s", got.stdout, want)
	}
}

func TestConfigShowWithoutShares(t *testing.T) {
	t.Parallel()
	got := execute(t, "--config", t.TempDir(), "config", "show")
	if !strings.HasSuffix(got.stdout, "trusted_proxies: []\nshares: {}\n") {
		t.Errorf("output:\n%s", got.stdout)
	}
}

func TestConfigShowWriteFailure(t *testing.T) {
	t.Parallel()
	var errOut bytes.Buffer
	opts := options{configDir: t.TempDir(), command: commandShow}
	status := runConfig(opts, systemDirs(), errWriter{}, &errOut)
	if status != exitError || !strings.Contains(errOut.String(), "disk full") {
		t.Errorf("status %d stderr %q", status, errOut.String())
	}
}

// fetch returns the body of a GET request.
func fetch(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// closeBody closes the body of a response, and fails the test when that does not work.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close the response body: %v", err)
	}
}
