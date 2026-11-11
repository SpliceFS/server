package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/BlueRival/splicefs/internal/wire"
	"github.com/BlueRival/splicefs/server/auth"
)

func login(e *env, body string) response {
	return e.do(request{method: "POST", target: "/api/v1/login", body: strings.NewReader(body)})
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := login(e, `{"user":"alice","password":"pw"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var lr wire.LoginResponse
	decodeJSON(t, resp, &lr)
	if lr.Token != "new-token" || !lr.Expires.Equal(testTime.Add(time.Hour)) {
		t.Errorf("response = %+v", lr)
	}
	wantHeader(t, resp, "Cache-Control", "no-store")
	if len(e.loginCalls) != 1 || e.loginCalls[0] != (loginCall{"192.0.2.1", "alice", "pw"}) {
		t.Errorf("login calls = %+v", e.loginCalls)
	}
}

func TestLoginAcceptsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if resp := login(e, " \n{\"user\":\"a\",\"password\":\"b\"}\n"); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestLoginMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, body string }{
		{"empty", ""},
		{"blank", "  \n"},
		{"not JSON", "user=alice"},
		{"truncated", `{"user":"alice"`},
		{"array", `[{"user":"a","password":"b"}]`},
		{"string", `"alice"`},
		{"null", `null`},
		{"unknown field", `{"user":"a","password":"b","admin":true}`},
		{"wrong type", `{"user":1,"password":"b"}`},
		{"two objects", `{"user":"a","password":"b"}{"user":"c","password":"d"}`},
		{"trailing text", `{"user":"a","password":"b"} x`},
		{"too large", `{"user":"a","password":"` + strings.Repeat("p", maxLoginBody) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wantError(t, login(e, tt.body), http.StatusBadRequest, "malformed login request")
			if len(e.loginCalls) != 0 {
				t.Errorf("Login was called: %+v", e.loginCalls)
			}
		})
	}
}

func TestLoginBodyAtLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	prefix := `{"user":"a","password":"`
	filler := strings.Repeat("p", maxLoginBody-len(prefix)-len(`"}`))
	if resp := login(e, prefix+filler+`"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestLoginBodyReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.do(request{method: "POST", target: "/api/v1/login", body: iotest.ErrReader(errors.New("connection reset"))})
	wantError(t, resp, http.StatusBadRequest, "malformed login request")
}

func TestLoginFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		status     int
		message    string
		retryAfter string
		logged     bool
	}{
		{"bad credentials", auth.ErrBadCredentials, http.StatusUnauthorized, "bad credentials", "", false},
		{"wrapped bad credentials", errors.Join(errors.New("x"), auth.ErrBadCredentials), http.StatusUnauthorized, "bad credentials", "", false},
		{"rate limited", auth.ErrRateLimited, http.StatusTooManyRequests, "too many login attempts", "60", false},
		{"other", errors.New("read user: disk on fire"), http.StatusInternalServerError, "internal error", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.loginResult = func() (string, time.Time, error) { return "", time.Time{}, tt.err }
			resp := login(e, `{"user":"alice","password":"pw"}`)
			wantError(t, resp, tt.status, tt.message)
			wantHeader(t, resp, "Retry-After", tt.retryAfter)
			wantHeader(t, resp, "WWW-Authenticate", "")
			if got := len(e.logged("request failed")) == 1; got != tt.logged {
				t.Errorf("error logged = %v, want %v", got, tt.logged)
			}
		})
	}
}

func TestLoginClientAddress(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.TrustedProxies = trusted("192.0.2.0/24") })
	e.do(request{
		method: "POST", target: "/api/v1/login", body: strings.NewReader(`{"user":"a","password":"b"}`),
		remote: "192.0.2.9:80", header: map[string]string{"X-Forwarded-For": "198.51.100.7, 192.0.2.5"},
	})
	if len(e.loginCalls) != 1 || e.loginCalls[0].clientAddr != "198.51.100.7" {
		t.Errorf("login calls = %+v", e.loginCalls)
	}
}

func TestLoginSecretsAreNotLogged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	login(e, `{"user":"alice","password":"hunter2-secret"}`)
	e.get("/api/v1/nodes", tokenAll)
	if out := e.logs.String(); strings.Contains(out, "hunter2-secret") || strings.Contains(out, tokenAll) || strings.Contains(out, "Bearer") {
		t.Errorf("log contains a secret:\n%s", out)
	}
}
