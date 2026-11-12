package main

import (
	"bytes"
	"context"
	"errors"
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
)

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

// execute runs the program with no input and returns its status and output.
func execute(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	status = run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return status, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	t.Parallel()
	status, stdout, stderr := execute(t, "--version")
	if want := "splicefsd " + version.String() + "\n"; status != exitOK || stdout != want || stderr != "" {
		t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, stdout, stderr, want)
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		status, stdout, stderr := execute(t, arg)
		if status != exitOK || stdout != usage || stderr != "" {
			t.Errorf("%s: status %d stdout %q stderr %q", arg, status, stdout, stderr)
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
			status, stdout, stderr := execute(t, tt.args...)
			if status != exitUsage || stdout != "" {
				t.Errorf("status %d stdout %q", status, stdout)
			}
			if !strings.HasPrefix(stderr, "splicefsd: ") || !strings.Contains(stderr, tt.want) || !strings.HasSuffix(stderr, usage) {
				t.Errorf("stderr = %q", stderr)
			}
		})
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
			status := run(context.Background(), []string{"hash-password"}, strings.NewReader(tt.input), &out, &errOut)
			if status != exitOK || errOut.Len() != 0 {
				t.Fatalf("status %d stderr %q", status, errOut.String())
			}
			hash := out.String()
			if !strings.HasSuffix(hash, "\n") || strings.Count(hash, "\n") != 1 || !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
				t.Fatalf("output = %q", hash)
			}
			hash = strings.TrimSuffix(hash, "\n")
			if ok, err := auth.VerifyPassword(hash, tt.want); err != nil || !ok {
				t.Errorf("the hash does not verify %q: %v, %v", tt.want, ok, err)
			}
			if ok, _ := auth.VerifyPassword(hash, tt.want+"x"); ok {
				t.Error("the hash verifies another password")
			}
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
		{"empty input", strings.NewReader(""), io.Discard, strings.NewReader("0123456789abcdef"), "empty password"},
		{"empty line", strings.NewReader("\n"), io.Discard, strings.NewReader("0123456789abcdef"), "empty password"},
		{"unreadable input", iotest.ErrReader(errors.New("terminal gone")), io.Discard, strings.NewReader("0123456789abcdef"), "read password: terminal gone"},
		{"no randomness", strings.NewReader("pw\n"), io.Discard, strings.NewReader("short"), "read salt"},
		{"unwritable output", strings.NewReader("pw\n"), errWriter{}, strings.NewReader("0123456789abcdef"), "write hash: disk full"},
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

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

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
			logger := newLogger(tt.cfg, &buf)
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

func TestLogListening(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logListening(slog.New(slog.NewTextHandler(&buf, nil)),
		[]net.Addr{&net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 8480}, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 11), Port: 8481}})
	out := buf.String()
	if strings.Count(out, "listening") != 2 || !strings.Contains(out, "192.0.2.10:8480") || !strings.Contains(out, "192.0.2.11:8481") {
		t.Errorf("log = %q", out)
	}
}

// freePort returns a loopback port that nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
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

// serverDirs writes a configuration directory and a state directory for a server on a free
// loopback port, and returns them with the port.
func serverDirs(t *testing.T) (configDir, stateDir string, port int) {
	t.Helper()
	port = freePort(t)
	root := t.TempDir()
	configDir, stateDir = filepath.Join(root, "config"), filepath.Join(root, "state")
	writeFiles(t, configDir, map[string]string{
		"conf.d/10-server.yaml": "listen:\n  - address: 127.0.0.1\n    port: " + strconv.Itoa(port) + "\n" +
			"    plain: true\n" +
			"cluster:\n  name: example\n  node: east\nlog:\n  level: info\n  format: json\n",
		"shares.d/media.yaml": "path: " + filepath.Join(root, "media") + "\n",
	})
	writeFiles(t, filepath.Join(root, "media"), map[string]string{"hello.txt": "hello"})
	writeFiles(t, stateDir, map[string]string{
		"shared/keys.yaml": "keys:\n  - id: k\n    secret: \"" +
			strings.Repeat("QUJD", 11) + "\"\n",
		"shared/nodes.yaml": "cluster: example\nnodes:\n  - name: east\n" +
			"    url: https://east.example.com/api\n",
	})
	return configDir, stateDir, port
}

// watchedLog collects what the server logs, and tells when it has started listening.
type watchedLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newWatchedLog() *watchedLog { return &watchedLog{ready: make(chan struct{})} }

func (w *watchedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), `"msg":"listening"`) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func (w *watchedLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// startServer runs the server in the background and waits until it listens.
func startServer(
	ctx context.Context, t *testing.T, configDir, stateDir string,
) (*watchedLog, <-chan int) {
	t.Helper()
	logs := newWatchedLog()
	status := make(chan int, 1)
	go func() {
		args := []string{"--config", configDir, "--state", stateDir}
		status <- run(ctx, args, strings.NewReader(""), io.Discard, logs)
	}()
	select {
	case <-logs.ready:
	case code := <-status:
		t.Fatalf("the server exited with status %d: %s", code, logs)
	}
	return logs, status
}

func TestServerRunsUntilCancelled(t *testing.T) {
	t.Parallel()
	configDir, stateDir, port := serverDirs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs, status := startServer(ctx, t, configDir, stateDir)

	body := fetch(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/v1/health")
	if want := `"version":"` + version.String() + `"`; !strings.Contains(body, want) || !strings.Contains(body, `"node":"east"`) {
		t.Errorf("health = %s", body)
	}
	cancel()
	if code := <-status; code != exitOK {
		t.Errorf("exit status = %d, want 0\n%s", code, logs)
	}
	if !strings.Contains(logs.String(), `"msg":"request"`) {
		t.Errorf("no access log:\n%s", logs)
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
			writeFiles(t, configDir, map[string]string{"conf.d/20-bad.yaml": "listen: []\n"})
		}, "invalid server config"},
		{"unreadable configuration", func(t *testing.T, configDir, _ string) {
			writeFiles(t, configDir, map[string]string{"conf.d/20-bad.yaml": "unknown: 1\n"})
		}, "read server config"},
		{"share directory is missing", func(t *testing.T, configDir, _ string) {
			missing := map[string]string{"shares.d/media.yaml": "path: /nonexistent/media\n"}
			writeFiles(t, configDir, missing)
		}, `share "media"`},
		{"no keys", func(t *testing.T, _, stateDir string) {
			if err := os.Remove(filepath.Join(stateDir, "shared", "keys.yaml")); err != nil {
				t.Fatal(err)
			}
		}, "check keys file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			configDir, stateDir, _ := serverDirs(t)
			tt.mutate(t, configDir, stateDir)
			var out, errOut bytes.Buffer
			args := []string{"--config", configDir, "--state", stateDir}
			status := run(context.Background(), args, strings.NewReader(""), &out, &errOut)
			if status != exitError || out.Len() != 0 {
				t.Errorf("status %d stdout %q", status, out.String())
			}
			if got := errOut.String(); !strings.HasPrefix(got, "splicefsd: ") || !strings.Contains(got, tt.want) || !strings.HasSuffix(got, "\n") {
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
	configDir, _, _ := serverDirs(t)
	for _, dir := range []string{configDir, filepath.Join(t.TempDir(), "empty")} {
		status, stdout, stderr := execute(t, "--config", dir, "config", "check")
		if status != exitOK || stdout != "configuration ok\n" || stderr != "" {
			t.Errorf("%s: status %d stdout %q stderr %q", dir, status, stdout, stderr)
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
	status, stdout, stderr := execute(t, "--config", dir, "config", "check")
	if status != exitError || stdout != "" {
		t.Errorf("status %d stdout %q", status, stdout)
	}
	wanted := []string{"log.level", "base_path", "shares.m.path", "shares.Bad", "a.yaml", "m.yaml"}
	for _, want := range wanted {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
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
	status, stdout, stderr := execute(t, "--config", dir, "config", "show")
	if status != exitOK || stderr != "" {
		t.Fatalf("status %d stderr %q", status, stderr)
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
	if stdout != want {
		t.Errorf("output:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestConfigShowWithoutShares(t *testing.T) {
	t.Parallel()
	_, stdout, _ := execute(t, "--config", t.TempDir(), "config", "show")
	if !strings.HasSuffix(stdout, "trusted_proxies: []\nshares: {}\n") {
		t.Errorf("output:\n%s", stdout)
	}
}

func TestConfigShowWriteFailure(t *testing.T) {
	t.Parallel()
	var errOut bytes.Buffer
	opts := options{configDir: t.TempDir(), command: commandShow}
	status := runConfig(opts, systemDirs, errWriter{}, &errOut)
	if status != exitError || !strings.Contains(errOut.String(), "disk full") {
		t.Errorf("status %d stderr %q", status, errOut.String())
	}
}

// fetch returns the body of a GET request.
func fetch(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
