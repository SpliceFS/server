//go:build unix

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

// The tests in this file send a real signal to the test process. They are not parallel,
// so that no other server of this test binary is running when the signal arrives.
//
//nolint:paralleltest // a signal goes to the whole test process
func TestServerStopsOnSignal(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			setup := setUpServer(t)
			server := startServer(context.Background(), t, setup)
			if body := fetch(t, setup.healthURL()); body == "" {
				t.Error("empty health response")
			}
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			if code := <-server.status; code != exitOK {
				t.Errorf("exit status = %d, want 0\n%s", code, server.logs)
			}
		})
	}
}

// reloadLimit bounds every wait of the reload tests.
const reloadLimit = 5 * time.Second

// hangUp sends SIGHUP to the test process, where the running server has asked for it.
func hangUp(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
}

// waitForLog waits until the server has logged text.
func waitForLog(t *testing.T, logs *watchedLog, text string) {
	t.Helper()
	deadline := time.Now().Add(reloadLimit)
	for !strings.Contains(logs.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("the server did not log %q:\n%s", text, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reloadServer is a running server whose configuration a reload test changes.
type reloadServer struct {
	setup serverSetup
	logs  *watchedLog
}

// startReloadServer starts a server whose user may use every share.
func startReloadServer(t *testing.T) reloadServer {
	t.Helper()
	setup := setUpServer(t)
	writeFiles(t, setup.stateDir, map[string]string{
		"shared/users/alice.yaml": "password_hash: \"" + lowCostHash("secret") + "\"\nshares: [\"*\"]\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	server := startServer(ctx, t, setup)
	// A stopped server no longer asks for SIGHUP, so no later test reloads it.
	t.Cleanup(func() {
		cancel()
		<-server.status
	})
	return reloadServer{setup: setup, logs: server.logs}
}

// apiRequest sends a request to the API of a test server and returns the response,
// which the caller closes with closeBody.
func apiRequest(t *testing.T, method, url string, body io.Reader, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// login logs in as the user of setUpServer and returns the token.
func login(t *testing.T, base string) string {
	t.Helper()
	creds := strings.NewReader(`{"user":"alice","password":"secret"}`)
	resp := apiRequest(t, http.MethodPost, base+"login", creds, "")
	defer closeBody(t, resp)
	var response wire.LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil || response.Token == "" {
		t.Fatalf("login: %v, token %q", err, response.Token)
	}
	return response.Token
}

// shareNames logs in as the user of setUpServer and returns the names of the shares the
// server lists.
func shareNames(t *testing.T, port int) []string {
	t.Helper()
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/"
	resp := apiRequest(t, http.MethodGet, base+"tree/", http.NoBody, login(t, base))
	defer closeBody(t, resp)
	var listing wire.Listing
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	return names
}

//nolint:paralleltest // a signal goes to the whole test process
func TestSIGHUPAppliesANewShare(t *testing.T) {
	server := startReloadServer(t)
	if got := shareNames(t, server.setup.port); strings.Join(got, ",") != "media" {
		t.Fatalf("shares before the reload = %v", got)
	}
	music := t.TempDir()
	writeFiles(t, server.setup.configDir, map[string]string{"shares.d/music.yaml": "path: " + music + "\n"})

	hangUp(t)
	waitForLog(t, server.logs, `"msg":"configuration applied"`)

	if got := shareNames(t, server.setup.port); strings.Join(got, ",") != "media,music" {
		t.Errorf("shares after the reload = %v", got)
	}
}

//nolint:paralleltest // a signal goes to the whole test process
func TestSIGHUPWithAnInvalidFileKeepsServing(t *testing.T) {
	server := startReloadServer(t)
	writeFiles(t, server.setup.configDir, map[string]string{"shares.d/bad.yaml": "unknown: 1\n"})

	hangUp(t)
	waitForLog(t, server.logs, "configuration not applied")

	if got := shareNames(t, server.setup.port); strings.Join(got, ",") != "media" {
		t.Errorf("shares after the refused reload = %v", got)
	}
	if body := fetch(t, server.setup.healthURL()); body == "" {
		t.Error("empty health response")
	}
}

// The server logs no debug line, so the new level shows by what it hides. After a reload to
// error, the "configuration applied" line of that same reload is the first one to go.
//
//nolint:paralleltest // a signal goes to the whole test process
func TestSIGHUPChangesTheLogLevel(t *testing.T) {
	server := startReloadServer(t)
	writeFiles(t, server.setup.configDir, map[string]string{
		"conf.d/20-quiet.yaml": "log:\n  level: error\n",
		"shares.d/music.yaml":  "path: " + t.TempDir() + "\n",
	})
	hangUp(t)
	// The share shows that the reload took effect. The next reload starts only after this
	// one has finished, so the level is already the new one when it logs.
	deadline := time.Now().Add(reloadLimit)
	for len(shareNames(t, server.setup.port)) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the reload did not take effect:\n%s", server.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFiles(t, server.setup.configDir, map[string]string{"shares.d/bad.yaml": "unknown: 1\n"})
	hangUp(t)
	waitForLog(t, server.logs, "configuration not applied")

	if strings.Contains(server.logs.String(), "configuration applied") {
		t.Errorf("an info line was logged after the reload to error:\n%s", server.logs)
	}
}
