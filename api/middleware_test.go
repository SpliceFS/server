package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

func trusted(prefixes ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, netip.MustParsePrefix(prefix))
	}
	return out
}

func TestClientAddress(t *testing.T) {
	t.Parallel()
	proxies := trusted("192.0.2.0/24", "2001:db8::/32")
	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"direct client", "198.51.100.7:5000", nil, "198.51.100.7"},
		{"direct client ignores header", "198.51.100.7:5000", []string{"203.0.113.9"}, "198.51.100.7"},
		{"trusted proxy, one entry", "192.0.2.1:80", []string{"203.0.113.9"}, "203.0.113.9"},
		{"rightmost untrusted entry", "192.0.2.1:80", []string{"10.0.0.1, 203.0.113.9, 192.0.2.77"}, "203.0.113.9"},
		{
			"client cannot pick an address left of the proxies", "192.0.2.1:80",
			[]string{"1.2.3.4, 203.0.113.9"},
			"203.0.113.9",
		},
		{"spaces", "192.0.2.1:80", []string{" 203.0.113.9 ,  192.0.2.4 "}, "203.0.113.9"},
		{"several header lines", "192.0.2.1:80", []string{"1.2.3.4", "203.0.113.9, 192.0.2.4"}, "203.0.113.9"},
		{"no header", "192.0.2.1:80", nil, "192.0.2.1"},
		{"empty header", "192.0.2.1:80", []string{""}, "192.0.2.1"},
		{"all entries trusted", "192.0.2.1:80", []string{"192.0.2.3, 192.0.2.4"}, "192.0.2.1"},
		{"unparsable entry", "192.0.2.1:80", []string{"203.0.113.9, garbage"}, "192.0.2.1"},
		{"unparsable entry left of a client", "192.0.2.1:80", []string{"garbage, 203.0.113.9"}, "203.0.113.9"},
		{"IPv6 client", "[2001:db8::1]:80", []string{"2001:4860::1"}, "2001:4860::1"},
		{"IPv6 direct", "[2001:4860::1]:80", nil, "2001:4860::1"},
		{"mapped IPv4 remote is trusted", "[::ffff:192.0.2.1]:80", []string{"203.0.113.9"}, "203.0.113.9"},
		{"mapped IPv4 entry", "192.0.2.1:80", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"remote without port", "198.51.100.7", nil, "198.51.100.7"},
		{"remote that is not an address", "@", nil, "@"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientAddress(r, proxies); got != tt.want {
				t.Errorf("clientAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveClientStoresAddress(t *testing.T) {
	t.Parallel()
	var got string
	h := resolveClient(nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = clientAddr(r.Context())
	}))
	r := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	r.RemoteAddr = "198.51.100.7:1"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "198.51.100.7" {
		t.Errorf("client = %q", got)
	}
	if got := clientAddr(r.Context()); got != "" {
		t.Errorf("client of a context without one = %q", got)
	}
}

// panicNodes is a NodeLister that panics, to reach the 500 answer of recoverPanics.
type panicNodes struct{}

func (panicNodes) Nodes() (wire.Nodes, error) { panic("kaboom") }

func TestServerVersionOnEveryAnswer(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t, func(o *Options) {
		o.Version = "v9.8.7"
		o.Nodes = panicNodes{}
	})
	tests := []struct {
		name   string
		method string
		target string
		token  string
		status int
	}{
		{"normal route", http.MethodGet, "/api/v1/health", "", http.StatusOK},
		{"unknown path", http.MethodGet, "/api/v1/nothing", "", http.StatusNotFound},
		{"wrong method", http.MethodPost, "/api/v1/health", "", http.StatusMethodNotAllowed},
		{"panicking handler", http.MethodGet, "/api/v1/nodes", tokenAll, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := fixture.do(&request{method: tt.method, target: tt.target, token: tt.token})
			_ = body(t, resp)
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if got := resp.Header.Get(serverVersionHeader); got != "v9.8.7" {
				t.Errorf("%s = %q, want v9.8.7", serverVersionHeader, got)
			}
		})
	}
}

func TestNoSniff(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler := noSniff(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestRecoverPanics(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t)
	logger := slog.New(slog.NewJSONHandler(fixture.logs, nil))
	h := noSniff(recoverPanics(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom: secret detail")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/boom", nil))
	wantError(t, response{rec.Result()}, http.StatusInternalServerError, "internal error")
	records := fixture.logged("panic in handler")
	if len(records) != 1 {
		t.Fatalf("log = %s", fixture.logs)
	}
	record := records[0]
	if record["level"] != "ERROR" || record["panic"] != "kaboom: secret detail" ||
		record["path"] != "/boom" || record["method"] != "GET" {
		t.Errorf("record = %v", record)
	}
	if stack, ok := record["stack"].(string); !ok || !strings.Contains(stack, "TestRecoverPanics") {
		t.Errorf("stack = %q", stack)
	}
}

func TestRecoverPanicsWithErrorValue(t *testing.T) {
	t.Parallel()
	h := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(errors.New("some error"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRecoverPanicsPassesAbortOn(t *testing.T) {
	t.Parallel()
	handler := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		recovered := recover()
		if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", recovered)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	t.Error("no panic")
}

func TestRecoverPanicsLeavesNormalRequestsAlone(t *testing.T) {
	t.Parallel()
	h := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d", rec.Code)
	}
}

// steppingClock returns a time that is step later on every call.
func steppingClock(step time.Duration) func() time.Time {
	now := testTime()
	return func() time.Time {
		now = now.Add(step)
		return now
	}
}

func TestLogAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  float64
		bytes   float64
		user    string
	}{
		{"explicit status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			mustWrite(w, "12345")
		}, 404, 5, ""},
		{"implicit status", func(w http.ResponseWriter, _ *http.Request) {
			mustWrite(w, "abc")
			mustWrite(w, "de")
		}, 200, 5, ""},
		{"no body", func(http.ResponseWriter, *http.Request) {}, 200, 0, ""},
		{"first status wins", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusTeapot)
		}, 202, 0, ""},
		{"user", func(_ http.ResponseWriter, r *http.Request) {
			setAccessUser(r.Context(), "alice")
		}, 200, 0, "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newEnv(t)
			logger := slog.New(slog.NewJSONHandler(fixture.logs, nil))
			handler := resolveClient(nil, logAccess(logger, steppingClock(250*time.Millisecond), tt.handler))
			req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/x?token=abc", nil)
			req.RemoteAddr = "198.51.100.7:99"
			req.Header.Set("Authorization", "Bearer secret-token")
			handler.ServeHTTP(httptest.NewRecorder(), req)
			records := fixture.logged("request")
			if len(records) != 1 {
				t.Fatalf("log = %s", fixture.logs)
			}
			wantAccessRecord(t, records[0], tt.status, tt.bytes)
			wantAccessUser(t, records[0], tt.user)
			if out := fixture.logs.String(); strings.Contains(out, "secret-token") || strings.Contains(out, "abc") {
				t.Errorf("log leaks request data: %s", out)
			}
		})
	}
}

// wantAccessRecord checks the fields of an access log record that every request has.
func wantAccessRecord(t *testing.T, record map[string]any, status, size float64) {
	t.Helper()
	want := map[string]any{
		"level": "INFO", "method": "GET", "path": "/api/v1/x", "status": status,
		"bytes": size, "duration": float64(250 * time.Millisecond), "client": "198.51.100.7",
	}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %v, want %v", key, record[key], value)
		}
	}
}

// wantAccessUser checks that the record names user, or has no user field if user is empty.
func wantAccessUser(t *testing.T, record map[string]any, user string) {
	t.Helper()
	got, present := record["user"]
	if (user == "") == present || (present && got != user) {
		t.Errorf("user = %v, want %q", got, user)
	}
}

// mustWrite writes text to w. A recorder never refuses a write, so an error means the
// test is broken.
func mustWrite(w io.Writer, text string) {
	if _, err := io.WriteString(w, text); err != nil {
		panic(err)
	}
}

func TestSetAccessUserWithoutLog(t *testing.T) {
	t.Parallel()
	setAccessUser(httptest.NewRequestWithContext(context.Background(), "GET", "/", nil).Context(), "alice")
}

func TestAccessLogOfPanickingRequestShowsStatus500(t *testing.T) {
	t.Parallel()
	fixture := newEnv(t, func(o *Options) { o.Shares["media"] = panicShare{} })
	resp := fixture.get("/api/v1/space/media", tokenAll)
	wantError(t, resp, http.StatusInternalServerError, "internal error")
	records := fixture.logged("request")
	if len(records) != 1 || records[0]["status"] != float64(500) {
		t.Errorf("log = %s", fixture.logs)
	}
}

type panicShare struct{ Share }

func (panicShare) Space() (wire.Space, error) { panic("share exploded") }

func TestStatusRecorderUnwrap(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if got := (&statusRecorder{ResponseWriter: rec}).Unwrap(); got != http.ResponseWriter(rec) {
		t.Errorf("Unwrap = %v", got)
	}
	if err := http.NewResponseController(&statusRecorder{ResponseWriter: rec}).Flush(); err != nil {
		t.Errorf("Flush through the recorder: %v", err)
	}
}

func TestStatusRecorderPassesWriteErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("broken pipe")
	rec := &statusRecorder{ResponseWriter: failingWriter{err: want}}
	if _, err := rec.Write([]byte("x")); !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
	if rec.bytes != 0 {
		t.Errorf("bytes = %d", rec.bytes)
	}
}

// failingWriter is a ResponseWriter whose body writes fail.
type failingWriter struct {
	http.ResponseWriter

	err error
}

func (failingWriter) Header() http.Header         { return http.Header{} }
func (failingWriter) WriteHeader(int)             {}
func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
