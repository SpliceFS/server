package api

import (
	"net/http"
	"testing"

	"github.com/SpliceFS/common/wire"
)

func TestHealth(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.do(&request{target: "/api/v1/health"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var h wire.Health
	decodeJSON(t, resp, &h)
	if h != (wire.Health{Status: "ok", Version: "v1.2.3", Node: "east"}) {
		t.Errorf("health = %+v", h)
	}
	wantHeader(t, resp, "X-Content-Type-Options", "nosniff")
	wantHeader(t, resp, "Cache-Control", "")
}

func TestEmptyBasePath(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t, func(o *Options) { o.BasePath = "" })
	if resp := fixture.do(&request{target: "/v1/health"}); resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/health status = %d", resp.StatusCode)
	}
	wantError(t, fixture.do(&request{target: "/api/v1/health"}), http.StatusNotFound, "not found")
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	targets := []string{
		"/", "/v1/health", "/api", "/api/", "/api/v1", "/api/v1/", "/api/v2/health",
		"/api/v1/health/", "/api/v1/unknown", "/api/v1/login/x",
		"/api/v1/tree", "/api/v1/file", "/api/v1/file/", "/api/v1/file/media",
		"/api/v1/space", "/api/v1/space/", "/api/v1/space/media/",
		"/api/v1/file/media/../blob.zzz", "/api/v1/file/media/./blob.zzz",
		"/api/v1/file//blob.zzz", "/api/v1/file/media//blob.zzz", "/api/v1/tree//media",
		"*",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			wantError(t, fixture.get(target, tokenAll), http.StatusNotFound, "not found")
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	tests := []struct {
		method, target, allow string
	}{
		{"POST", "/api/v1/health", "GET, HEAD"},
		{"PUT", "/api/v1/health", "GET, HEAD"},
		{"GET", "/api/v1/login", "POST"},
		{"DELETE", "/api/v1/login", "POST"},
		{"POST", "/api/v1/nodes", "GET, HEAD"},
		{"POST", "/api/v1/tree/", "GET, HEAD"},
		{"POST", "/api/v1/tree/media", "GET, HEAD"},
		{"POST", "/api/v1/tree/media/sub/", "GET, HEAD"},
		{"POST", "/api/v1/file/media/blob.zzz", "GET, HEAD"},
		{"PUT", "/api/v1/file/media/blob.zzz", "GET, HEAD"},
		{"DELETE", "/api/v1/space/media", "GET, HEAD"},
		{"OPTIONS", "/api/v1/space/media", "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			t.Parallel()
			resp := fixture.do(&request{method: tt.method, target: tt.target, token: tokenAll})
			wantError(t, resp, http.StatusMethodNotAllowed, "method not allowed")
			wantHeader(t, resp, "Allow", tt.allow)
		})
	}
}

func TestHeadIsRoutedToGetEndpoints(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	resp := fixture.do(&request{method: "HEAD", target: "/api/v1/space/media", token: tokenAll})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	wantHeader(t, resp, "Content-Type", jsonType)
}

func TestIsCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/a/b/", true},
		{"/a/b.c/.d", true},
		{"/a/..b", true},
		{"", false},
		{"*", false},
		{"a/b", false},
		{"/a//b", false},
		{"/a/./b", false},
		{"/a/../b", false},
		{"/a/b/..", false},
		{"/.", false},
	}
	for _, tt := range tests {
		if got := isCanonical(tt.path); got != tt.want {
			t.Errorf("isCanonical(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResponseHeadersOfEveryEndpoint(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	targets := []string{
		"/api/v1/nodes", "/api/v1/tree/", "/api/v1/tree/media", "/api/v1/space/media",
		"/api/v1/file/media/blob.zzz",
	}
	for _, target := range targets {
		resp := fixture.get(target, tokenAll)
		wantHeader(t, resp, "Cache-Control", "private, no-cache")
		wantHeader(t, resp, "X-Content-Type-Options", "nosniff")
		body(t, resp)
	}
}
