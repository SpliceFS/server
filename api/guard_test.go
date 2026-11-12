package api

import (
	"net/http"
	"testing"

	"github.com/SpliceFS/common/wire"
)

func TestTokenRequired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
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
		for _, h := range headers {
			t.Run(h.name+" "+target, func(t *testing.T) {
				t.Parallel()
				header := map[string]string{}
				if h.value != "" {
					header["Authorization"] = h.value
				}
				resp := e.do(request{target: target, header: header})
				wantError(t, resp, http.StatusUnauthorized, h.status)
				wantHeader(t, resp, "WWW-Authenticate", "Bearer")
				wantHeader(t, resp, "Cache-Control", "")
			})
		}
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.do(request{target: "/api/v1/nodes", header: map[string]string{"Authorization": "bEaReR " + tokenAll}})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestVerifyFailureIsInternalError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/nodes", tokenBroken)
	wantError(t, resp, http.StatusInternalServerError, "internal error")
	wantHeader(t, resp, "WWW-Authenticate", "")
	records := e.logged("request failed")
	if len(records) != 1 || records[0]["level"] != "ERROR" {
		t.Fatalf("log = %v", e.logRecords())
	}
	if got, _ := records[0]["error"].(string); got != errBroken.Error() {
		t.Errorf("logged error = %q, want %q", got, errBroken)
	}
}

func TestShareAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
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
			target := route + tt.share
			if route != "/api/v1/space/" {
				target += "/"
			}
			t.Run(tt.name+" "+route, func(t *testing.T) {
				t.Parallel()
				resp := e.get(target, tt.token)
				if tt.msg == "" {
					if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
						t.Errorf("status = %d", resp.StatusCode)
					}
					return
				}
				wantError(t, resp, tt.status, tt.msg)
				wantHeader(t, resp, "Cache-Control", "private, no-cache")
			})
		}
	}
}

func TestNodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/nodes", tokenNone)
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
	e := newEnv(t)
	e.nodes.err = errBroken
	wantError(t, e.get("/api/v1/nodes", tokenAll), http.StatusInternalServerError, "internal error")
	if len(e.logged("request failed")) != 1 {
		t.Errorf("log = %v", e.logRecords())
	}
}

func TestSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/space/media", tokenMedia)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var sp wire.Space
	decodeJSON(t, resp, &sp)
	if sp != (wire.Space{Total: 1000, Free: 400}) {
		t.Errorf("space = %+v", sp)
	}
	e.media.errs["space"] = errBroken
	wantError(t, e.get("/api/v1/space/media", tokenMedia), http.StatusInternalServerError, "internal error")
}
