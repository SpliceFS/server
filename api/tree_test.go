package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SpliceFS/server/internal/wire"
	"github.com/SpliceFS/server/share"
)

func TestListShares(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		want  []string
	}{
		{"one share", tokenMedia, []string{"media"}},
		{"every share, sorted", tokenAll, []string{"docs", "media"}},
		{"no share", tokenNone, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			resp := e.get("/api/v1/tree/", tt.token)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			raw := body(t, resp)
			if tt.want == nil && !strings.Contains(raw, `"entries":[]`) {
				t.Errorf("body = %s, want an empty array", raw)
			}
			var l wire.Listing
			decodeBody(t, raw, &l)
			var names []string
			for _, entry := range l.Entries {
				names = append(names, entry.Name)
				if !entry.Dir || entry.Size != 0 || !entry.MTime.Equal(testTime) || entry.ETag != "" {
					t.Errorf("entry = %+v", entry)
				}
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("names = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestListSharesStatError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.media.errs["stat"] = errBroken
	wantError(t, e.get("/api/v1/tree/", tokenAll), http.StatusInternalServerError, "internal error")
	if len(e.logged("request failed")) != 1 {
		t.Errorf("log = %s", e.logs)
	}
}

func TestListDirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, target := range []string{
		"/api/v1/tree/media", "/api/v1/tree/media/", "/api/v1/tree/media/sub", "/api/v1/tree/media/sub/",
	} {
		resp := e.get(target, tokenMedia)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", target, resp.StatusCode)
		}
		wantHeader(t, resp, "Cache-Control", "private, no-cache")
		var l wire.Listing
		decodeJSON(t, resp, &l)
		if strings.HasSuffix(strings.TrimSuffix(target, "/"), "/sub") {
			if l.Entries == nil || len(l.Entries) != 0 {
				t.Errorf("%s: entries = %#v, want an empty non-nil slice", target, l.Entries)
			}
			continue
		}
		want := []wire.Entry{
			{Name: "blob.zzz", Size: 10, MTime: testTime, ETag: share.ETag(10, testTime)},
			{Name: "sub", Dir: true, MTime: testTime},
		}
		if len(l.Entries) != 2 || l.Entries[0] != want[0] || l.Entries[1] != want[1] {
			t.Errorf("%s: entries = %+v", target, l.Entries)
		}
	}
}

func TestListDirectoryEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.media.dirs["sub/empty"] = nil
	raw := body(t, e.get("/api/v1/tree/media/sub/empty", tokenMedia))
	if strings.TrimSpace(raw) != `{"entries":[]}` {
		t.Errorf("body = %s", raw)
	}
}

func TestListDirectoryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		target  string
		err     error
		status  int
		message string
	}{
		{"missing", "/api/v1/tree/media/nope", nil, http.StatusNotFound, "not found"},
		{"a file", "/api/v1/tree/media/blob.zzz", nil, http.StatusNotFound, "not found"},
		{"double trailing slash", "/api/v1/tree/media/sub//", nil, http.StatusNotFound, "not found"},
		{"other error", "/api/v1/tree/media/sub", errors.New("EIO"), http.StatusInternalServerError, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if tt.err != nil {
				e.media.errs["list"] = tt.err
			}
			wantError(t, e.get(tt.target, tokenMedia), tt.status, tt.message)
		})
	}
}
