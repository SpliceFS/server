package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SpliceFS/common/wire"
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
			fixture := newEnv(t)
			resp := fixture.get("/api/v1/tree/", tt.token)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			raw := body(t, resp)
			if tt.want == nil && !strings.Contains(raw, `"entries":[]`) {
				t.Errorf("body = %s, want an empty array", raw)
			}
			var listing wire.Listing
			decodeBody(t, raw, &listing)
			if names := shareDirNames(t, listing); strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("names = %v, want %v", names, tt.want)
			}
		})
	}
}

// shareDirNames returns the names of the entries of the share list, and checks that each
// entry is a bare directory.
func shareDirNames(t *testing.T, listing wire.Listing) []string {
	t.Helper()
	names := make([]string, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
		if !entry.Dir || entry.Size != 0 || !entry.MTime.Equal(testTime()) || entry.ETag != "" {
			t.Errorf("entry = %+v", entry)
		}
	}
	return names
}

func TestListSharesStatError(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	fixture.media.errs["stat"] = errBroken
	wantError(t, fixture.get("/api/v1/tree/", tokenAll), http.StatusInternalServerError, "internal error")
	if len(fixture.logged("request failed")) != 1 {
		t.Errorf("log = %s", fixture.logs)
	}
}

func TestListDirectory(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	for _, target := range []string{
		"/api/v1/tree/media", "/api/v1/tree/media/", "/api/v1/tree/media/sub", "/api/v1/tree/media/sub/",
	} {
		resp := fixture.get(target, tokenMedia)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", target, resp.StatusCode)
		}
		wantHeader(t, resp, "Cache-Control", "private, no-cache")
		var listing wire.Listing
		decodeJSON(t, resp, &listing)
		if strings.HasSuffix(strings.TrimSuffix(target, "/"), "/sub") {
			if listing.Entries == nil || len(listing.Entries) != 0 {
				t.Errorf("%s: entries = %#v, want an empty non-nil slice", target, listing.Entries)
			}
			continue
		}
		want := []wire.Entry{
			{Name: "blob.zzz", Size: 10, MTime: testTime(), ETag: share.ETag(10, testTime())},
			{Name: "sub", Dir: true, MTime: testTime()},
		}
		if len(listing.Entries) != 2 || listing.Entries[0] != want[0] || listing.Entries[1] != want[1] {
			t.Errorf("%s: entries = %+v", target, listing.Entries)
		}
	}
}

func TestListDirectoryEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	fixture.media.dirs["sub/empty"] = nil
	raw := body(t, fixture.get("/api/v1/tree/media/sub/empty", tokenMedia))
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
			fixture := newEnv(t)
			if tt.err != nil {
				fixture.media.errs["list"] = tt.err
			}
			wantError(t, fixture.get(tt.target, tokenMedia), tt.status, tt.message)
		})
	}
}
