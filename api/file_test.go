package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/SpliceFS/server/internal/wire"
	"github.com/SpliceFS/server/share"
)

var blobETag = share.ETag(10, testTime)

const lastModified = "Sun, 01 Nov 2026 12:00:00 GMT"

func TestGetFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/file/media/blob.zzz", tokenMedia)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := body(t, resp); got != "0123456789" {
		t.Errorf("body = %q", got)
	}
	for name, want := range map[string]string{
		"ETag":                   blobETag,
		"Last-Modified":          lastModified,
		"Content-Type":           "application/octet-stream",
		"Content-Length":         "10",
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "private, no-cache",
		"X-Content-Type-Options": "nosniff",
		"Vary":                   "",
		"Content-Encoding":       "",
	} {
		wantHeader(t, resp, name, want)
	}
}

func TestGetNestedAndEmptyFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/file/media/sub/inner.zzz", tokenMedia)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != "inner" {
		t.Errorf("nested: status %d body %q", resp.StatusCode, got)
	}
	resp = e.get("/api/v1/file/media/empty.zzz", tokenMedia)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != "" {
		t.Errorf("empty: status %d body %q", resp.StatusCode, got)
	}
}

func TestGetFileClosesFile(t *testing.T) {
	t.Parallel()
	var opened []*fakeFile
	e := newEnv(t)
	e.handler = NewHandler(Options{
		BasePath: "/api", Auth: e.auth, Shares: map[string]Share{"media": trackingShare{e.media, &opened}},
	})
	for _, req := range []struct{ target, header, value string }{
		{"blob.zzz", "Range", "bytes=0-0, 2-2"},
		{"blob.zzz", "Range", "bytes=0-1"},
		{"blob.zzz", "X-None", ""},
		{"page.html", "Accept-Encoding", "gzip"},
	} {
		resp := e.get("/api/v1/file/media/"+req.target, tokenMedia, req.header, req.value)
		body(t, resp)
	}
	if len(opened) != 4 {
		t.Fatalf("opened %d files", len(opened))
	}
	for i, f := range opened {
		if !*f.closed {
			t.Errorf("file %d was not closed", i)
		}
	}
}

// trackingShare remembers the files it opens.
type trackingShare struct {
	*fakeShare
	opened *[]*fakeFile
}

func (s trackingShare) Open(name string) (File, error) {
	f, err := s.fakeShare.Open(name)
	if err == nil {
		*s.opened = append(*s.opened, f.(*fakeFile))
	}
	return f, err
}

func TestGetFileRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		header       []string
		status       int
		body         string
		contentRange string
	}{
		{"first bytes", []string{"Range", "bytes=0-3"}, 206, "0123", "bytes 0-3/10"},
		{"open ended", []string{"Range", "bytes=7-"}, 206, "789", "bytes 7-9/10"},
		{"suffix", []string{"Range", "bytes=-2"}, 206, "89", "bytes 8-9/10"},
		{"trailing comma is one range", []string{"Range", "bytes=1-2,"}, 206, "12", "bytes 1-2/10"},
		{"matching If-Match", []string{"Range", "bytes=0-1", "If-Match", blobETag}, 206, "01", "bytes 0-1/10"},
		{"matching If-Range", []string{"Range", "bytes=0-1", "If-Range", blobETag}, 206, "01", "bytes 0-1/10"},
		{"stale If-Range sends everything", []string{"Range", "bytes=0-1", "If-Range", `"stale"`}, 200, "0123456789", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			resp := e.get("/api/v1/file/media/blob.zzz", tokenMedia, tt.header...)
			if got := body(t, resp); resp.StatusCode != tt.status || got != tt.body {
				t.Errorf("status %d body %q, want %d %q", resp.StatusCode, got, tt.status, tt.body)
			}
			wantHeader(t, resp, "Content-Range", tt.contentRange)
			wantHeader(t, resp, "ETag", blobETag)
		})
	}
}

func TestGetFileRangeRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, header, contentRange string }{
		{"two ranges", "bytes=0-1,3-4", "bytes */10"},
		{"many ranges", "bytes=0-0,1-1,2-2,3-3", "bytes */10"},
		{"past the end", "bytes=20-", "bytes */10"},
		{"garbage", "bytes=abc", ""},
		{"other unit", "items=0-1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			resp := e.get("/api/v1/file/media/blob.zzz", tokenMedia, "Range", tt.header)
			wantError(t, resp, http.StatusRequestedRangeNotSatisfiable, "requested range not satisfiable")
			wantHeader(t, resp, "Content-Range", tt.contentRange)
		})
	}
}

func TestGetFilePreconditions(t *testing.T) {
	t.Parallel()
	failing := map[string][]string{
		"If-Match of another version":              {"If-Match", `"a-b-c"`},
		"If-Match of another version with a range": {"Range", "bytes=0-1", "If-Match", `"a-b-c"`},
		"If-Match of a compressed tag":             {"If-Match", `"a-gzip"`, "Accept-Encoding", "gzip"},
	}
	for name, header := range failing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wantError(t, e.get("/api/v1/file/media/blob.zzz", tokenMedia, header...), http.StatusPreconditionFailed, "precondition failed")
		})
	}
	notModified := map[string][]string{
		"If-None-Match of this version":   {"If-None-Match", blobETag},
		"If-Modified-Since the same time": {"If-Modified-Since", lastModified},
	}
	for name, header := range notModified {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			resp := e.get("/api/v1/file/media/blob.zzz", tokenMedia, header...)
			if got := body(t, resp); resp.StatusCode != http.StatusNotModified || got != "" {
				t.Errorf("status %d body %q", resp.StatusCode, got)
			}
		})
	}
}

func TestGetFileErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		err    error
		status int
		msg    string
	}{
		{"missing", "/api/v1/file/media/nope.zzz", nil, 404, "not found"},
		{"directory", "/api/v1/file/media/sub", nil, 404, "not found"},
		{"share root", "/api/v1/file/media/", nil, 404, "not found"},
		{"open fails", "/api/v1/file/media/blob.zzz", errors.New("EIO"), 500, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.media.dirs["sub"] = nil
			e.media.dirs[""] = nil
			if tt.err != nil {
				e.media.errs["open"] = tt.err
			}
			wantError(t, e.get(tt.target, tokenMedia), tt.status, tt.msg)
		})
	}
}

func TestGetFileSeekFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.handler = NewHandler(Options{
		BasePath: "/api", Auth: e.auth, Shares: map[string]Share{"media": unseekableShare{e.media}},
	})
	wantError(t, e.get("/api/v1/file/media/blob.zzz", tokenMedia), http.StatusInternalServerError, "internal error")
}

// unseekableShare opens files that cannot seek.
type unseekableShare struct{ *fakeShare }

func (s unseekableShare) Open(name string) (File, error) {
	f, err := s.fakeShare.Open(name)
	if err != nil {
		return nil, err
	}
	return unseekableFile{f.(*fakeFile)}, nil
}

type unseekableFile struct{ *fakeFile }

func (unseekableFile) Seek(int64, int) (int64, error) { return 0, errors.New("no seeking") }

func TestHeadFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.do(request{method: "HEAD", target: "/api/v1/file/media/blob.zzz", token: tokenMedia})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for name, want := range map[string]string{
		"ETag":                   blobETag,
		"Last-Modified":          lastModified,
		"Content-Length":         "10",
		"Accept-Ranges":          "bytes",
		"Content-Type":           "application/octet-stream",
		wire.HeaderHeadDigest:    "digest-of-blob.zzz",
		"Cache-Control":          "private, no-cache",
		"X-Content-Type-Options": "nosniff",
		"Content-Encoding":       "",
		"Vary":                   "",
	} {
		wantHeader(t, resp, name, want)
	}
	if got := body(t, resp); got != "" {
		t.Errorf("body = %q", got)
	}
}

func TestHeadCompressibleFileVariesOnEncoding(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.do(request{
		method: "HEAD", target: "/api/v1/file/media/page.html", token: tokenMedia,
		header: map[string]string{"Accept-Encoding": "gzip"},
	})
	wantHeader(t, resp, "Content-Type", "text/html; charset=utf-8")
	wantHeader(t, resp, "Vary", "Accept-Encoding")
	wantHeader(t, resp, "Content-Encoding", "")
	wantHeader(t, resp, "Content-Length", strconv.Itoa(13*200))
	wantHeader(t, resp, "ETag", share.ETag(13*200, testTime))
}

func TestHeadFileErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		err    error
		status int
		msg    string
	}{
		{"missing", "/api/v1/file/media/nope.zzz", nil, 404, "not found"},
		{"directory", "/api/v1/file/media/sub", nil, 404, "not found"},
		{"digest fails", "/api/v1/file/media/blob.zzz", errors.New("EIO"), 500, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.media.dirs["sub"] = nil
			if tt.err != nil {
				e.media.errs["head"] = tt.err
			}
			wantError(t, e.do(request{method: "HEAD", target: tt.target, token: tokenMedia}), tt.status, tt.msg)
		})
	}
}

func TestContentTypeOf(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"a.html":      "text/html; charset=utf-8",
		"a.HTML":      "text/html; charset=utf-8",
		"a.json":      "application/json",
		"a.svg":       "image/svg+xml",
		"a.zzz":       "application/octet-stream",
		"noextension": "application/octet-stream",
		"a.tar.zzz":   "application/octet-stream",
	}
	for name, want := range tests {
		if got := contentTypeOf(name); got != want {
			t.Errorf("contentTypeOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMultipleRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"bytes=0-1", false},
		{"bytes=0-1,", false},
		{"bytes=,0-1", false},
		{"bytes= 0-1 , ", false},
		{"bytes=", false},
		{"bytes=0-1,2-3", true},
		{"bytes=0-1, 2-3, 4-5", true},
		{"items=0-1,2-3", false},
		{"Bytes=0-1,2-3", false},
	}
	for _, tt := range tests {
		if got := multipleRanges(tt.header); got != tt.want {
			t.Errorf("multipleRanges(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestErrorBodyWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := &errorBodyWriter{ResponseWriter: rec}
	if w.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap does not return the wrapped writer")
	}
	w.WriteHeader(http.StatusPartialContent)
	if n, err := io.WriteString(w, "part"); n != 4 || err != nil {
		t.Errorf("Write = %d, %v", n, err)
	}
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "part" {
		t.Errorf("status %d body %q", rec.Code, rec.Body)
	}
}
