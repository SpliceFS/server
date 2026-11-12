//go:build unix

package main

import (
	"context"
	"strconv"
	"syscall"
	"testing"
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
