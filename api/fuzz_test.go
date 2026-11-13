package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func FuzzParseLogin(f *testing.F) {
	for _, seed := range []string{
		`{"user":"alice","password":"pw"}`, ` {"user":"a","password":"b"} `, `{}`,
		`{"user":"a","password":"b","extra":1}`, `{"user":1}`, `null`, `[]`, `"x"`, `42`, ``,
		`{"user":"a"}{"user":"b"}`, `{"user":"a"} x`, `{"user":"a"`, "{\x00}", `{"user":"\ud800"}`,
		`{"user":"a","user":"b"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := parseLogin(data)
		if err != nil {
			// The handler discards the partial value of a refused body, so only the error
			// matters; nothing else is promised about it.
			return
		}
		// Only a JSON object may become a login attempt; "null" and arrays must not.
		if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
			t.Fatalf("parseLogin(%q) accepted a body that is not an object", data)
		}
		// What was read must survive being written and read again, so no field is lost.
		again, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := parseLogin(again); err != nil || back != req {
			t.Fatalf("parseLogin(%q) = %+v, then %+v, %v", data, req, back, err)
		}
	})
}

func FuzzBearerToken(f *testing.F) {
	for _, seed := range []string{
		"Bearer abc", "bearer abc", "BEARER abc", "Bearer ", "Bearer", "", "Basic abc",
		"Bearer a b", "Bearer a\tb", "Bearer  abc", "Bearerabc", "Bearer a.b.c", "Bearer \x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		token, ok := bearerToken(header)
		if !ok {
			return
		}
		// The token is exactly what follows the scheme, so nothing is trimmed or guessed.
		if token == "" || header != header[:len(bearerPrefix)]+token {
			t.Fatalf("bearerToken(%q) = %q", header, token)
		}
		// Whitespace inside a token would let two different headers name the same token.
		if strings.ContainsAny(token, " \t") {
			t.Fatalf("bearerToken(%q) returned a token with whitespace", header)
		}
	})
}

func FuzzAuthorizationHeader(f *testing.F) {
	for _, seed := range []string{
		"Bearer " + tokenMedia, "bearer " + tokenAll, "Bearer " + tokenExpired,
		"Bearer " + tokenBroken, "Bearer nope", "Bearer", "", "Basic " + tokenMedia,
		"Bearer " + tokenMedia + " x", "Bearer\t" + tokenMedia, "Bearer " + tokenMedia + "\x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		fixture := newEnv(t)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
			"/api/v1/nodes", nil)
		req.Header["Authorization"] = []string{header}
		rec := httptest.NewRecorder()
		fixture.handler.ServeHTTP(rec, req)
		token, ok := bearerToken(header)
		_, known := fixture.auth.tokens[token]
		// A request is served only for a header that names a token the server issued.
		if rec.Code == http.StatusOK && (!ok || !known) {
			t.Fatalf("Authorization %q was accepted", header)
		}
		// Whatever the header holds, the server must answer, never fail on its own.
		if rec.Code >= 500 && (!ok || token != tokenBroken) {
			t.Fatalf("Authorization %q gave status %d", header, rec.Code)
		}
	})
}

func FuzzFileRequest(f *testing.F) {
	for _, seed := range [][4]string{
		{"blob.zzz", "bytes=0-4", "", ""},
		{"blob.zzz", "bytes=5-", "", ""},
		{"blob.zzz", "bytes=-3", "", ""},
		{"blob.zzz", "bytes=0-1,3-4", "", ""},
		{"blob.zzz", "bytes=9-3", "", ""},
		{"blob.zzz", "bytes=99999999999999999999-", "", ""},
		{"blob.zzz", "bytes=-0", "", ""},
		{"blob.zzz", "bytes=", "", ""},
		{"blob.zzz", "items=1-2", "", ""},
		{"blob.zzz", "bytes=1-2", "\"x\"", ""},
		{"page.html", "", "", "gzip"},
		{"page.html", "", "", "gzip;q=0, *;q=1"},
		{"sub/inner.zzz", "", "", ""},
		{"sub", "", "", ""},
		{"../blob.zzz", "", "", ""},
		{"%2e%2e/blob.zzz", "", "", ""},
		{"a%00b", "", "", ""},
		{"a%2Fb", "", "", ""},
		{"sub//inner.zzz", "", "", ""},
		{"nope", "", "", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	f.Fuzz(func(t *testing.T, name, byteRange, ifRange, encoding string) {
		fixture := newEnv(t)
		target := "/api/v1/file/media/" + name
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
		if err != nil || req.URL.Path == "" {
			t.Skip("not a request a client can send")
		}
		req.Header.Set("Authorization", "Bearer "+tokenMedia)
		req.Header.Set("Range", byteRange)
		req.Header.Set("If-Range", ifRange)
		req.Header.Set("Accept-Encoding", encoding)
		rec := httptest.NewRecorder()
		fixture.handler.ServeHTTP(rec, req)
		// The server answers every crafted request; a 5xx means a panic or a missed case.
		if rec.Code >= 500 {
			t.Fatalf("GET %q Range %q gave status %d", target, byteRange, rec.Code)
		}
		// A body can never be larger than the largest file, whatever range was asked.
		if rec.Code == http.StatusPartialContent && rec.Body.Len() > 4096 {
			t.Fatalf("GET %q Range %q sent %d bytes", target, byteRange, rec.Body.Len())
		}
	})
}

func FuzzMultipleRanges(f *testing.F) {
	for _, seed := range []string{
		"bytes=0-1", "bytes=0-1,3-4", "bytes=0-1,", "bytes=,", "bytes=", "", "items=0-1,2-3",
		"bytes= 0-1 , 2-3", "BYTES=0-1,2-3", ",,,", "bytes=0-1,,2-3",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		got := multipleRanges(header)
		// Without the bytes unit nothing is a multi-range request, so single ranges
		// and other units still reach ServeContent unchanged.
		if got && !strings.HasPrefix(header, "bytes=") {
			t.Fatalf("multipleRanges(%q) true without the bytes unit", header)
		}
		// More than one range needs a separator between the specs.
		if got && !strings.Contains(header, ",") {
			t.Fatalf("multipleRanges(%q) true without a comma", header)
		}
	})
}

func FuzzAcceptEncoding(f *testing.F) {
	for _, seed := range []string{
		"gzip", "gzip;q=0", "gzip;q=0.5", "*", "*;q=0", "identity", "gzip, deflate", "GZIP",
		"gzip;q=abc", "gzip;q=", ";q=1", ",,", "gzip;q=1;q=0", "gzip;q=NaN", "gzip;q=-1",
		"gzip;q=1e400", "*;q=Inf",
	} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		h := http.Header{"Accept-Encoding": {first, second}}
		got := acceptsGzip(h)
		// A coding that cannot be read must never switch compression on, so gzip or a
		// wildcard has to appear in the headers for it to be granted.
		joined := strings.ToLower(first + second)
		if got && !strings.Contains(joined, "gzip") && !strings.Contains(joined, "*") {
			t.Fatalf("acceptsGzip(%q, %q) true without gzip or *", first, second)
		}
	})
}

func FuzzIsCanonical(f *testing.F) {
	for _, seed := range []string{
		"/", "/a", "/a/b", "/a/", "//a", "/a//b", "/.", "/..", "/a/./b", "/a/../b", "", "a",
		"/a/.b", "/a/b.", "/%2e%2e", "/a/...", "/\x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, urlPath string) {
		if !isCanonical(urlPath) {
			return
		}
		// The mux redirects any other form, and an API has no use for redirects.
		if !strings.HasPrefix(urlPath, "/") || strings.Contains(urlPath, "//") ||
			strings.Contains(urlPath, "/./") || strings.Contains(urlPath, "/../") ||
			strings.HasSuffix(urlPath, "/.") || strings.HasSuffix(urlPath, "/..") {
			t.Fatalf("isCanonical(%q) true", urlPath)
		}
	})
}
