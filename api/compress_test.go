package api

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/BlueRival/splicefs/internal/wire"
	"github.com/BlueRival/splicefs/server/share"
)

func gunzip(t *testing.T, resp response) string {
	t.Helper()
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"gzip", []string{"gzip"}, true},
		{"upper case", []string{"GZip"}, true},
		{"list", []string{"deflate, gzip, br"}, true},
		{"quality", []string{"gzip;q=0.5"}, true},
		{"spaces around quality", []string{"gzip ; q = 0.8"}, true},
		{"upper case q", []string{"gzip;Q=0.5"}, true},
		{"q=1", []string{"gzip;q=1.0"}, true},
		{"q=0", []string{"gzip;q=0"}, false},
		{"q=0.000", []string{"gzip;q=0.000"}, false},
		{"q=0 among others", []string{"deflate, gzip;q=0, br"}, false},
		{"unreadable quality", []string{"gzip;q=high"}, false},
		{"negative quality", []string{"gzip;q=-1"}, false},
		{"other parameter", []string{"gzip;level=9"}, true},
		{"identity only", []string{"identity"}, false},
		{"other codings", []string{"br, deflate"}, false},
		{"gzip as part of a name", []string{"x-gzip, gzipped"}, false},
		{"wildcard", []string{"*"}, true},
		{"wildcard refused", []string{"*;q=0"}, false},
		{"explicit gzip beats wildcard", []string{"gzip;q=0, *"}, false},
		{"explicit gzip beats wildcard, other order", []string{"*;q=0, gzip"}, true},
		{"wildcard with other coding", []string{"br, *;q=0.1"}, true},
		{"several header lines", []string{"br", "gzip"}, true},
		{"several header lines, refused", []string{"gzip", "gzip;q=0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for _, v := range tt.values {
				h.Add("Accept-Encoding", v)
			}
			if got := acceptsGzip(h); got != tt.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", tt.values, got, tt.want)
			}
		})
	}
}

func TestCompressible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		contentType string
		want        bool
	}{
		{"text/plain; charset=utf-8", true},
		{"text/html", true},
		{"text/csv", true},
		{"application/json", true},
		{"application/xml", true},
		{"application/javascript", true},
		{"image/svg+xml", true},
		{"application/ld+json", true},
		{"application/atom+xml", true},
		{"APPLICATION/JSON; charset=utf-8", true},
		{"application/octet-stream", false},
		{"application/pdf", false},
		{"image/png", false},
		{"video/mp4", false},
		{"application/zip", false},
		{"", false},
		{"not a media type", false},
	}
	for _, tt := range tests {
		if got := compressible(tt.contentType); got != tt.want {
			t.Errorf("compressible(%q) = %v, want %v", tt.contentType, got, tt.want)
		}
	}
}

func TestGetCompressedFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	plain := e.media.files["page.html"].content
	resp := e.get("/api/v1/file/media/page.html", tokenMedia, "Accept-Encoding", "gzip, deflate")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := gunzip(t, resp); got != plain {
		t.Errorf("decompressed body differs: %d bytes, want %d", len(got), len(plain))
	}
	wantETag := strings.TrimSuffix(share.ETag(int64(len(plain)), testTime), `"`) + `-gzip"`
	for name, want := range map[string]string{
		"Content-Encoding":       "gzip",
		"Vary":                   "Accept-Encoding",
		"ETag":                   wantETag,
		"Content-Type":           "text/html; charset=utf-8",
		"Last-Modified":          lastModified,
		"Content-Length":         "",
		"Accept-Ranges":          "",
		"Cache-Control":          "private, no-cache",
		"X-Content-Type-Options": "nosniff",
	} {
		wantHeader(t, resp, name, want)
	}
}

func TestGetFileIsNotCompressedWhen(t *testing.T) {
	t.Parallel()
	html := "/api/v1/file/media/page.html"
	tests := []struct {
		name   string
		target string
		header []string
		status int
		vary   string
	}{
		{"the client does not ask", html, nil, 200, "Accept-Encoding"},
		{"gzip is refused", html, []string{"Accept-Encoding", "gzip;q=0"}, 200, "Accept-Encoding"},
		{"only identity is accepted", html, []string{"Accept-Encoding", "identity"}, 200, "Accept-Encoding"},
		{"there is a Range", html, []string{"Accept-Encoding", "gzip", "Range", "bytes=0-9"}, 206, "Accept-Encoding"},
		{"there is an If-Match", html, []string{"Accept-Encoding", "gzip", "If-Match", share.ETag(13*200, testTime)}, 200, "Accept-Encoding"},
		{"there is an If-None-Match", html, []string{"Accept-Encoding", "gzip", "If-None-Match", `"x"`}, 200, "Accept-Encoding"},
		{"there is an If-Range", html, []string{"Accept-Encoding", "gzip", "If-Range", `"x"`}, 200, "Accept-Encoding"},
		{"there is an If-Modified-Since", html, []string{"Accept-Encoding", "gzip", "If-Modified-Since", "Sun, 01 Nov 2020 12:00:00 GMT"}, 200, "Accept-Encoding"},
		{"the file is under 1 KiB", "/api/v1/file/media/small.html", []string{"Accept-Encoding", "gzip"}, 200, "Accept-Encoding"},
		{"the type is not compressible", "/api/v1/file/media/big.zzz", []string{"Accept-Encoding", "gzip"}, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			resp := e.get(tt.target, tokenMedia, tt.header...)
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			wantHeader(t, resp, "Content-Encoding", "")
			wantHeader(t, resp, "Vary", tt.vary)
			if strings.HasSuffix(resp.Header.Get("ETag"), `-gzip"`) {
				t.Errorf("ETag = %q", resp.Header.Get("ETag"))
			}
		})
	}
}

func TestGetFileCompressesAtExactlyOneKiB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for size, want := range map[int]string{1023: "", 1024: "gzip"} {
		e.media.files["edge.html"] = fileData{content: strings.Repeat("a", size)}
		resp := e.get("/api/v1/file/media/edge.html", tokenMedia, "Accept-Encoding", "gzip")
		body(t, resp)
		wantHeader(t, resp, "Content-Encoding", want)
	}
}

func TestGetCompressedFileReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.handler = NewHandler(Options{
		BasePath: "/api", Auth: e.auth, Shares: map[string]Share{"media": brokenReadShare{e.media}},
		Logger: e.logger(),
	})
	resp := e.get("/api/v1/file/media/page.html", tokenMedia, "Accept-Encoding", "gzip")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	body(t, resp)
	records := e.logged("compressed response ended early")
	if len(records) != 1 || !strings.Contains(records[0]["error"].(string), "disk failure") {
		t.Errorf("log = %s", e.logs)
	}
}

// brokenReadShare opens files whose Read fails.
type brokenReadShare struct{ *fakeShare }

func (s brokenReadShare) Open(name string) (File, error) {
	f, err := s.fakeShare.Open(name)
	if err != nil {
		return nil, err
	}
	ff := f.(*fakeFile)
	ff.ReadSeeker = readSeeker{Reader: iotest.ErrReader(errors.New("disk failure"))}
	return ff, nil
}

type readSeeker struct{ io.Reader }

func (readSeeker) Seek(int64, int) (int64, error) { return 0, nil }

func TestWriteGzipFailingClient(t *testing.T) {
	t.Parallel()
	want := errors.New("broken pipe")
	for _, src := range []string{"", strings.Repeat("data", 1000)} {
		if err := writeGzip(failingWriter{err: want}, strings.NewReader(src)); !errors.Is(err, want) {
			t.Errorf("source of %d bytes: err = %v, want broken pipe", len(src), err)
		}
	}
}

func TestListingsAreCompressedAboveOneKiB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var infos []share.Info
	for i := range 100 {
		name := "file-with-a-long-name-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + ".zzz"
		infos = append(infos, share.Info{Name: name, Size: int64(i), MTime: testTime, ETag: share.ETag(int64(i), testTime)})
	}
	e.media.dirs["big"] = infos
	resp := e.get("/api/v1/tree/media/big/", tokenMedia, "Accept-Encoding", "gzip")
	wantHeader(t, resp, "Content-Encoding", "gzip")
	wantHeader(t, resp, "Vary", "Accept-Encoding")
	wantHeader(t, resp, "Content-Type", jsonType)
	wantHeader(t, resp, "Content-Length", "")
	var l wire.Listing
	decodeBody(t, gunzip(t, resp), &l)
	if len(l.Entries) != 100 || l.Entries[5].Name != infos[5].Name {
		t.Errorf("listing has %d entries", len(l.Entries))
	}

	plain := e.get("/api/v1/tree/media/big/", tokenMedia)
	wantHeader(t, plain, "Content-Encoding", "")
	wantHeader(t, plain, "Vary", "Accept-Encoding")
	raw := body(t, plain)
	if plain.Header.Get("Content-Length") == "" || len(raw) < minCompressSize {
		t.Errorf("plain listing: Content-Length %q, %d bytes", plain.Header.Get("Content-Length"), len(raw))
	}
	refused := e.get("/api/v1/tree/media/big/", tokenMedia, "Accept-Encoding", "gzip;q=0")
	wantHeader(t, refused, "Content-Encoding", "")
}

func TestSmallJSONIsNotCompressed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp := e.get("/api/v1/space/media", tokenMedia, "Accept-Encoding", "gzip")
	wantHeader(t, resp, "Content-Encoding", "")
	wantHeader(t, resp, "Vary", "Accept-Encoding")
	wantHeader(t, resp, "Content-Length", "25")
	if got := body(t, resp); got != `{"total":1000,"free":400}` {
		t.Errorf("body = %q", got)
	}
}

func TestLargeNodeListIsCompressed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for i := range 40 {
		e.nodes.nodes.Nodes = append(e.nodes.nodes.Nodes, wire.Node{
			Name: "node" + strings.Repeat("a", i), URL: "https://node.example.com/api",
		})
	}
	resp := e.get("/api/v1/nodes", tokenMedia, "Accept-Encoding", "gzip")
	wantHeader(t, resp, "Content-Encoding", "gzip")
	var nodes wire.Nodes
	decodeBody(t, gunzip(t, resp), &nodes)
	if len(nodes.Nodes) != 41 {
		t.Errorf("%d nodes", len(nodes.Nodes))
	}
}

func TestWriteJSONEncodeFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := &server{log: e.logger()}
	h := noSniff(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, r, make(chan int))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	wantError(t, response{rec.Result()}, http.StatusInternalServerError, "internal error")
	if len(e.logged("request failed")) != 1 {
		t.Errorf("log = %s", e.logs)
	}
}

func TestWriteGzip(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeGzip(rec, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	resp := response{rec.Result()}
	if got := gunzip(t, resp); got != "hello" || resp.StatusCode != http.StatusOK {
		t.Errorf("status %d body %q", resp.StatusCode, got)
	}
	wantHeader(t, resp, "Content-Encoding", "gzip")
}
