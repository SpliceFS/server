package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/share"
)

var testTime = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// fileData is a file of a fakeShare.
type fileData struct {
	content string
}

// fakeShare is an in-memory Share. An entry of errs, keyed by operation name, makes that
// operation fail with the error.
type fakeShare struct {
	files map[string]fileData
	dirs  map[string][]share.Info
	space wire.Space
	errs  map[string]error
}

func (f *fakeShare) info(name string, d fileData) share.Info {
	return share.Info{
		Name:  name[strings.LastIndex(name, "/")+1:],
		Size:  int64(len(d.content)),
		MTime: testTime,
		ETag:  share.ETag(int64(len(d.content)), testTime),
	}
}

func (f *fakeShare) Stat(string) (share.Info, error) {
	if err := f.errs["stat"]; err != nil {
		return share.Info{}, err
	}
	return share.Info{Name: ".", Dir: true, MTime: testTime}, nil
}

func (f *fakeShare) List(name string) ([]share.Info, error) {
	if err := f.errs["list"]; err != nil {
		return nil, err
	}
	if _, ok := f.files[name]; ok {
		return nil, share.ErrNotDir
	}
	infos, ok := f.dirs[name]
	if !ok {
		return nil, share.ErrNotFound
	}
	return infos, nil
}

func (f *fakeShare) lookup(name string) (fileData, error) {
	if _, ok := f.dirs[name]; ok {
		return fileData{}, share.ErrIsDir
	}
	d, ok := f.files[name]
	if !ok {
		return fileData{}, share.ErrNotFound
	}
	return d, nil
}

func (f *fakeShare) Open(name string) (File, error) {
	if err := f.errs["open"]; err != nil {
		return nil, err
	}
	d, err := f.lookup(name)
	if err != nil {
		return nil, err
	}
	return &fakeFile{ReadSeeker: strings.NewReader(d.content), info: f.info(name, d), closed: new(bool)}, nil
}

func (f *fakeShare) HeadDigest(name string) (string, share.Info, error) {
	if err := f.errs["head"]; err != nil {
		return "", share.Info{}, err
	}
	d, err := f.lookup(name)
	if err != nil {
		return "", share.Info{}, err
	}
	return "digest-of-" + name, f.info(name, d), nil
}

func (f *fakeShare) Space() (wire.Space, error) {
	if err := f.errs["space"]; err != nil {
		return wire.Space{}, err
	}
	return f.space, nil
}

// fakeFile is a File over a ReadSeeker.
type fakeFile struct {
	io.ReadSeeker
	info   share.Info
	closed *bool
}

func (f *fakeFile) Close() error     { *f.closed = true; return nil }
func (f *fakeFile) Info() share.Info { return f.info }

// fakeAuth is an Authenticator with fixed tokens.
type fakeAuth struct {
	tokens    map[string]auth.Claims
	verifyErr map[string]error
	login     func(ctx context.Context, clientAddr, user, password string) (string, time.Time, error)
}

func (a *fakeAuth) Login(ctx context.Context, clientAddr, user, password string) (string, time.Time, error) {
	return a.login(ctx, clientAddr, user, password)
}

func (a *fakeAuth) Verify(token string) (auth.Claims, error) {
	if err := a.verifyErr[token]; err != nil {
		return auth.Claims{}, err
	}
	claims, ok := a.tokens[token]
	if !ok {
		return auth.Claims{}, auth.ErrInvalidToken
	}
	return claims, nil
}

// fakeNodes is a NodeLister.
type fakeNodes struct {
	nodes wire.Nodes
	err   error
}

func (n fakeNodes) Nodes() (wire.Nodes, error) { return n.nodes, n.err }

// Tokens known to fakeAuth.
const (
	tokenMedia   = "token-media"   // alice, allowed: media
	tokenAll     = "token-all"     // bob, allowed: every share
	tokenNone    = "token-none"    // carol, allowed: nothing
	tokenExpired = "token-expired" // Verify answers auth.ErrExpiredToken
	tokenBroken  = "token-broken"  // Verify fails with an unexpected error
)

var errBroken = errors.New("keys file unreadable: /secret/path")

// env is a handler over fakes, and what they recorded.
type env struct {
	media, docs *fakeShare
	auth        *fakeAuth
	nodes       *fakeNodes
	logs        *bytes.Buffer
	handler     http.Handler
	loginCalls  []loginCall
	loginResult func() (string, time.Time, error)
}

type loginCall struct{ clientAddr, user, password string }

func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	e := &env{logs: new(bytes.Buffer)}
	e.media = &fakeShare{
		files: map[string]fileData{
			"blob.zzz":       {content: "0123456789"},
			"empty.zzz":      {content: ""},
			"sub/inner.zzz":  {content: "inner"},
			"page.html":      {content: strings.Repeat("<p>hello</p>\n", 200)},
			"small.html":     {content: "<p>small</p>"},
			"data.json":      {content: strings.Repeat(`{"k":"v"},`, 200)},
			"big.zzz":        {content: strings.Repeat("x", 4096)},
			"feed.atom+json": {content: "x"},
		},
		dirs: map[string][]share.Info{
			".": {
				{Name: "blob.zzz", Size: 10, MTime: testTime, ETag: share.ETag(10, testTime)},
				{Name: "sub", Dir: true, MTime: testTime},
			},
			"sub":       {},
			"sub/empty": nil,
		},
		space: wire.Space{Total: 1000, Free: 400},
		errs:  map[string]error{},
	}
	e.media.dirs[""] = e.media.dirs["."]
	e.docs = &fakeShare{files: map[string]fileData{}, dirs: map[string][]share.Info{}, errs: map[string]error{}}
	e.auth = &fakeAuth{
		tokens: map[string]auth.Claims{
			tokenMedia: {User: "alice", Shares: []string{"media"}},
			tokenAll:   {User: "bob", Shares: []string{"*"}},
			tokenNone:  {User: "carol"},
		},
		verifyErr: map[string]error{tokenExpired: auth.ErrExpiredToken, tokenBroken: errBroken},
	}
	e.loginResult = func() (string, time.Time, error) { return "new-token", testTime.Add(time.Hour), nil }
	e.auth.login = func(_ context.Context, addr, user, password string) (string, time.Time, error) {
		e.loginCalls = append(e.loginCalls, loginCall{addr, user, password})
		return e.loginResult()
	}
	e.nodes = &fakeNodes{nodes: wire.Nodes{Cluster: "example", Nodes: []wire.Node{{Name: "east", URL: "https://east.example.com/api"}}}}
	opts := Options{
		BasePath: "/api",
		Node:     "east",
		Version:  "v1.2.3",
		Shares:   map[string]Share{"media": e.media, "docs": e.docs},
		Auth:     e.auth,
		Nodes:    e.nodes,
		Logger:   slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mutate {
		m(&opts)
	}
	e.handler = NewHandler(opts)
	return e
}

// response is a recorded response. It is a struct of its own so that tests, which read
// every body through the helpers below, are not held to closing each one by hand.
type response struct{ *http.Response }

// request describes one test request.
type request struct {
	method string
	target string
	token  string
	header map[string]string
	body   io.Reader
	remote string
}

// do sends r to the handler and returns the response.
func (e *env) do(r request) response {
	if r.method == "" {
		r.method = http.MethodGet
	}
	req := httptest.NewRequestWithContext(context.Background(), r.method, r.target, r.body)
	req.RemoteAddr = "192.0.2.1:4321"
	if r.remote != "" {
		req.RemoteAddr = r.remote
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return response{rec.Result()}
}

// get is a GET with a token.
func (e *env) get(target, token string, header ...string) response {
	h := map[string]string{}
	for i := 0; i+1 < len(header); i += 2 {
		h[header[i]] = header[i+1]
	}
	return e.do(request{target: target, token: token, header: h})
}

// logRecords returns the log lines written so far.
func (e *env) logRecords() []map[string]any {
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(e.logs.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			panic(err)
		}
		records = append(records, rec)
	}
	return records
}

// logged returns the log records with the given message.
func (e *env) logged(msg string) []map[string]any {
	var out []map[string]any
	for _, rec := range e.logRecords() {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func body(t *testing.T, resp response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return string(data)
}

// decodeJSON decodes the body of resp into v.
func decodeJSON(t *testing.T, resp response, v any) {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != jsonType {
		t.Errorf("Content-Type = %q, want %q", got, jsonType)
	}
	if err := json.Unmarshal([]byte(body(t, resp)), v); err != nil {
		t.Fatal(err)
	}
}

// wantError checks the status and the wire.Error message of an error response.
func wantError(t *testing.T, resp response, status int, message string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("status = %d, want %d", resp.StatusCode, status)
	}
	var e wire.Error
	decodeJSON(t, resp, &e)
	if e.Error != message {
		t.Errorf("error message = %q, want %q", e.Error, message)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// wantHeader checks one response header.
func wantHeader(t *testing.T, resp response, name, want string) {
	t.Helper()
	if got := resp.Header.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// decodeBody decodes already read JSON.
func decodeBody(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("%v in %q", err, raw)
	}
}

// logger returns a logger that writes JSON lines to the log buffer of the env.
func (e *env) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
