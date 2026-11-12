//go:build unix

package main

import (
	"context"
	"encoding/json"
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
func TestServerStopsOnSignal(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			configDir, stateDir, port := serverDirs(t)
			logs, status := startServer(context.Background(), t, configDir, stateDir)
			if body := fetch(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/v1/health"); body == "" {
				t.Error("empty health response")
			}
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			if code := <-status; code != exitOK {
				t.Errorf("exit status = %d, want 0\n%s", code, logs)
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

// reloadFixture starts a server whose user may use every share, and returns what the
// reload tests need to change it.
func reloadFixture(t *testing.T) (logs *watchedLog, configDir string, port int) {
	t.Helper()
	configDir, stateDir, port := serverDirs(t)
	writeFiles(t, stateDir, map[string]string{
		"shared/users/alice.yaml": "password_hash: \"" + lowCostHash("secret") + "\"\nshares: [\"*\"]\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	logs, status := startServer(ctx, t, configDir, stateDir)
	// A stopped server no longer asks for SIGHUP, so no later test reloads it.
	t.Cleanup(func() {
		cancel()
		<-status
	})
	return logs, configDir, port
}

// shareNames logs in as the user of serverDirs and returns the names of the shares the
// server lists.
func shareNames(t *testing.T, port int) []string {
	t.Helper()
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/"
	creds := `{"user":"alice","password":"secret"}`
	req, err := http.NewRequestWithContext(
		context.Background(), "POST", base+"login", strings.NewReader(creds))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var login wire.LoginResponse
	err = json.NewDecoder(resp.Body).Decode(&login)
	_ = resp.Body.Close()
	if err != nil || login.Token == "" {
		t.Fatalf("login: %v, token %q", err, login.Token)
	}
	req, err = http.NewRequestWithContext(context.Background(), "GET", base+"tree/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+login.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var listing wire.Listing
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listing.Entries {
		names = append(names, e.Name)
	}
	return names
}

func TestSIGHUPAppliesANewShare(t *testing.T) {
	logs, configDir, port := reloadFixture(t)
	if got := shareNames(t, port); strings.Join(got, ",") != "media" {
		t.Fatalf("shares before the reload = %v", got)
	}
	music := t.TempDir()
	writeFiles(t, configDir, map[string]string{"shares.d/music.yaml": "path: " + music + "\n"})

	hangUp(t)
	waitForLog(t, logs, `"msg":"configuration applied"`)

	if got := shareNames(t, port); strings.Join(got, ",") != "media,music" {
		t.Errorf("shares after the reload = %v", got)
	}
}

func TestSIGHUPWithAnInvalidFileKeepsServing(t *testing.T) {
	logs, configDir, port := reloadFixture(t)
	writeFiles(t, configDir, map[string]string{"shares.d/bad.yaml": "unknown: 1\n"})

	hangUp(t)
	waitForLog(t, logs, "configuration not applied")

	if got := shareNames(t, port); strings.Join(got, ",") != "media" {
		t.Errorf("shares after the refused reload = %v", got)
	}
	if body := fetch(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/v1/health"); body == "" {
		t.Error("empty health response")
	}
}

// The server logs no debug line, so the new level shows by what it hides. After a reload to
// error, the "configuration applied" line of that same reload is the first one to go.
func TestSIGHUPChangesTheLogLevel(t *testing.T) {
	logs, configDir, port := reloadFixture(t)
	writeFiles(t, configDir, map[string]string{
		"conf.d/20-quiet.yaml": "log:\n  level: error\n",
		"shares.d/music.yaml":  "path: " + t.TempDir() + "\n",
	})
	hangUp(t)
	// The share shows that the reload took effect. The next reload starts only after this
	// one has finished, so the level is already the new one when it logs.
	deadline := time.Now().Add(reloadLimit)
	for len(shareNames(t, port)) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the reload did not take effect:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFiles(t, configDir, map[string]string{"shares.d/bad.yaml": "unknown: 1\n"})
	hangUp(t)
	waitForLog(t, logs, "configuration not applied")

	if strings.Contains(logs.String(), "configuration applied") {
		t.Errorf("an info line was logged after the reload to error:\n%s", logs)
	}
}
