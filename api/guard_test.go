package api

import (
	"net/http"
	"testing"

	"github.com/SpliceFS/common/wire"
)

func TestTokenRequired(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	targets := []string{
		"/api/v1/nodes", "/api/v1/tree/", "/api/v1/tree/media", "/api/v1/tree/media/sub/",
		"/api/v1/file/media/blob.zzz", "/api/v1/space/media", "/api/v1/space/ghost",
	}
	headers := []struct {
		name   string
		value  string
		status string
	}{
		{"missing", "", "invalid token"},
		{"other scheme", "Basic dXNlcjpwYXNz", "invalid token"},
		{"scheme only", "Bearer", "invalid token"},
		{"scheme and space only", "Bearer ", "invalid token"},
		{"spaces in token", "Bearer token-all extra", "invalid token"},
		{"unknown token", "Bearer nonsense", "invalid token"},
		{"expired", "Bearer " + tokenExpired, "token expired"},
	}
	for _, target := range targets {
		for _, authCase := range headers {
			t.Run(authCase.name+" "+target, func(t *testing.T) {
				t.Parallel()
				header := map[string]string{}
				if authCase.value != "" {
					header["Authorization"] = authCase.value
				}
				resp := fixture.do(&request{target: target, header: header})
				wantError(t, resp, http.StatusUnauthorized, authCase.status)
				wantHeader(t, resp, "WWW-Authenticate", "Bearer")
				wantHeader(t, resp, "Cache-Control", "")
			})
		}
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.do(&request{target: "/api/v1/nodes", header: map[string]string{"Authorization": "bEaReR " + tokenAll}})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestVerifyFailureIsInternalError(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.get("/api/v1/nodes", tokenBroken)
	wantError(t, resp, http.StatusInternalServerError, "internal error")
	wantHeader(t, resp, "WWW-Authenticate", "")
	records := fixture.logged("request failed")
	if len(records) != 1 || records[0]["level"] != "ERROR" {
		t.Fatalf("log = %v", fixture.logRecords())
	}
	if got, ok := records[0]["error"].(string); !ok || got != errBroken.Error() {
		t.Errorf("logged error = %q, want %q", got, errBroken)
	}
}

func TestShareAccess(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	tests := []struct {
		name   string
		token  string
		share  string
		status int
		msg    string
	}{
		{"allowed", tokenMedia, "media", http.StatusOK, ""},
		{"wildcard", tokenAll, "docs", http.StatusOK, ""},
		{"not allowed", tokenMedia, "docs", http.StatusForbidden, "share not allowed"},
		{"allowed nothing", tokenNone, "media", http.StatusForbidden, "share not allowed"},
		{"unknown share, not allowed", tokenMedia, "ghost", http.StatusForbidden, "share not allowed"},
		{"unknown share, wildcard", tokenAll, "ghost", http.StatusNotFound, "not found"},
	}
	for _, tt := range tests {
		for _, route := range []string{"/api/v1/space/", "/api/v1/tree/", "/api/v1/file/"} {
			target := shareTarget(route, tt.share)
			t.Run(tt.name+" "+route, func(t *testing.T) {
				t.Parallel()
				wantShareAnswer(t, fixture.get(target, tt.token), tt.status, tt.msg)
			})
		}
	}
}

// shareTarget is the URL of a share below a route. Only the space route has no trailing
// slash after the share name.
func shareTarget(route, shareName string) string {
	if route == "/api/v1/space/" {
		return route + shareName
	}
	return route + shareName + "/"
}

// wantShareAnswer checks the answer to a request for a share. An empty message means the
// request is allowed, so the answer must not be a refusal of the token.
func wantShareAnswer(t *testing.T, resp response, status int, message string) {
	t.Helper()
	if message == "" {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("status = %d", resp.StatusCode)
		}
		return
	}
	wantError(t, resp, status, message)
	wantHeader(t, resp, "Cache-Control", "private, no-cache")
}

func TestNodes(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.get("/api/v1/nodes", tokenNone)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var nodes wire.Nodes
	decodeJSON(t, resp, &nodes)
	if nodes.Cluster != "example" || len(nodes.Nodes) != 1 || nodes.Nodes[0].Name != "east" {
		t.Errorf("nodes = %+v", nodes)
	}
	wantHeader(t, resp, "Cache-Control", "private, no-cache")
}

func TestNodesError(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	fixture.nodes.err = errBroken
	wantError(t, fixture.get("/api/v1/nodes", tokenAll), http.StatusInternalServerError, "internal error")
	if len(fixture.logged("request failed")) != 1 {
		t.Errorf("log = %v", fixture.logRecords())
	}
}

func TestSpace(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.get("/api/v1/space/media", tokenMedia)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var sp wire.Space
	decodeJSON(t, resp, &sp)
	if sp != (wire.Space{Total: 1000, Free: 400}) {
		t.Errorf("space = %+v", sp)
	}
	fixture.media.errs["space"] = errBroken
	wantError(t, fixture.get("/api/v1/space/media", tokenMedia), http.StatusInternalServerError, "internal error")
}
