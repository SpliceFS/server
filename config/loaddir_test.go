package config

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/units"
)

var errNoHost = errors.New("no host")

func testHost() (string, error)    { return "Test-Host", nil }
func failingHost() (string, error) { return "", errNoHost }

// files maps "conf.d/name" or "shares.d/name" to content.
type files map[string]string

// writeDir creates a configuration directory holding the given files.
func writeDir(t *testing.T, f files) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range f {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// conf is a directory with one settings file, share one with a share named m.
func conf(content string) files  { return files{"conf.d/a.yaml": content} }
func share(content string) files { return files{"shares.d/m.yaml": content} }

func load(t *testing.T, f files) (*Settings, error) {
	t.Helper()
	return loadDir(writeDir(t, f), testHost)
}

func defaultSettings() *Settings {
	return &Settings{
		Listen:   []Endpoint{{Listener: Listener{Address: netip.IPv6Unspecified(), Port: 8480}}},
		BasePath: "/api",
		Cluster:  ClusterSettings{Name: "default", Node: "test-host"},
		Auth: AuthSettings{
			TokenTTL:  units.Duration(30 * time.Minute),
			LoginRate: Rate{Count: 10, Per: time.Minute},
		},
		MTimePrecision: units.Duration(time.Second),
		Log:            Log{Level: "info", Format: "text"},
		Shares:         map[string]Share{},
		Sources:        map[string]string{},
	}
}

func TestLoadDirDefaults(t *testing.T) {
	t.Parallel()
	tests := map[string]files{
		"empty directory":   {},
		"missing both":      {"other/x.yaml": "a: 1"},
		"empty subdirs":     {"conf.d/.keep": "", "shares.d/.keep": ""},
		"empty file":        {"conf.d/a.yaml": ""},
		"comment only file": {"conf.d/a.yaml": "# nothing\n"},
		"null document":     {"conf.d/a.yaml": "null\n"},
		"null values":       {"conf.d/a.yaml": "listen:\nauth:\nlog: {}\n"},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := load(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if want := defaultSettings(); !reflect.DeepEqual(got, want) {
				t.Errorf("got\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func TestLoadDirMissingOneDirectory(t *testing.T) {
	t.Parallel()
	got, err := load(t, conf("base_path: /x\n"))
	if err != nil || got.BasePath != "/x" || len(got.Shares) != 0 {
		t.Fatalf("conf.d only: %+v, %v", got, err)
	}
	got, err = load(t, share("path: /srv/m\n"))
	if err != nil || len(got.Shares) != 1 {
		t.Fatalf("shares.d only: %+v, %v", got, err)
	}
}

func TestLoadDirMerge(t *testing.T) {
	t.Parallel()
	got, err := load(t, files{
		"conf.d/20-b.yaml": "base_path: /b\nauth:\n  login_rate: 5/s\n" +
			"listen:\n  - {address: 192.0.2.2, port: 9000}\n",
		"conf.d/10-a.yaml": "base_path: /a\nauth:\n  token_ttl: 1h\nlisten:\n" +
			"  - {address: 192.0.2.1, port: 1}\n  - {address: 192.0.2.3, port: 3}\n" +
			"cluster: {name: x, node: n1}\n",
		"conf.d/05-first.yaml": "base_path: /first\nlog: {level: debug}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := defaultSettings()
	want.BasePath = "/b"
	want.Listen = []Endpoint{
		{Listener: Listener{Address: netip.MustParseAddr("192.0.2.2"), Port: 9000}},
	}
	want.Cluster = ClusterSettings{Name: "x", Node: "n1"}
	want.Auth = AuthSettings{
		TokenTTL:  units.Duration(time.Hour),
		LoginRate: Rate{Count: 5, Per: time.Second},
	}
	want.Log.Level = "debug"
	want.Sources = map[string]string{
		"base_path": "20-b.yaml", "auth.login_rate": "20-b.yaml", "auth.token_ttl": "10-a.yaml",
		"listen": "20-b.yaml", "cluster.name": "10-a.yaml", "cluster.node": "10-a.yaml",
		"log.level": "05-first.yaml",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestLoadDirByteOrder(t *testing.T) {
	t.Parallel()
	// "B" sorts before "a" in byte order, so a.yaml wins.
	got, err := load(t, files{"conf.d/a.yaml": "base_path: /a\n", "conf.d/B.yaml": "base_path: /B\n"})
	if err != nil || got.BasePath != "/a" || got.Sources["base_path"] != "a.yaml" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestLoadDirIgnoredEntries(t *testing.T) {
	t.Parallel()
	got, err := load(t, files{
		"conf.d/.hidden.yaml":    "bad: [",
		"conf.d/a.yaml~":         "bad: [",
		"conf.d/a.tmp":           "bad: [",
		"conf.d/a.yml":           "bad: [",
		"conf.d/x.yaml/in.yaml":  "bad: [",
		"conf.d/ok.yaml":         "base_path: /ok\n",
		"shares.d/.m.yaml":       "bad: [",
		"shares.d/m.yaml~":       "bad: [",
		"shares.d/m.txt":         "bad: [",
		"shares.d/d.yaml/p.yaml": "bad: [",
	})
	if err != nil || got.BasePath != "/ok" || len(got.Shares) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestLoadDirDecodeErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		f    files
		want []string
	}{
		"unknown key":      {conf("nope: 1\n"), []string{"a.yaml", "nope"}},
		"removed key":      {conf("shares: {}\n"), []string{"a.yaml", "shares"}},
		"removed nested":   {conf("auth: {users_dir: x}\n"), []string{"a.yaml", "users_dir"}},
		"two documents":    {conf("a: 1\n---\nb: 2\n"), []string{"a.yaml", "more than one"}},
		"bad second doc":   {conf("a: 1\n---\n[\n"), []string{"a.yaml"}},
		"wrong type":       {conf("listen: 1\n"), []string{"a.yaml", "cannot unmarshal"}},
		"bad rate":         {conf("auth: {login_rate: 5/d}\n"), []string{"a.yaml", "rate"}},
		"syntax":           {conf("["), []string{"a.yaml"}},
		"second file":      {files{"conf.d/a.yaml": "{}", "conf.d/b.yaml": "x: 1"}, []string{"b.yaml"}},
		"share unknown":    {share("path: /a\nname: m\n"), []string{"m.yaml", "name"}},
		"share two docs":   {share("path: /a\n---\npath: /b\n"), []string{"m.yaml", "more than one"}},
		"share wrong type": {share("path: [a]\n"), []string{"m.yaml"}},
		"both dirs": {files{"conf.d/a.yaml": "x: 1", "shares.d/m.yaml": "y: 1"},
			[]string{"a.yaml", "m.yaml"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := load(t, tt.f)
			if err == nil || got != nil {
				t.Fatalf("got %+v, %v", got, err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestLoadDirValidation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		f    files
		want []string // empty: valid
	}{
		"plain": {conf("listen: [{address: '::', port: 80, plain: true}]"), nil},
		"tls":   {conf("listen: [{address: '::', port: 80, tls: {cert_file: c, key_file: k}}]"), nil},
		"plain and tls": {conf("listen: [{address: '::', port: 80, plain: true,\n" +
			" tls: {cert_file: c, key_file: k}}]"),
			[]string{"listen[0].plain: must not be true together with tls", "(file a.yaml)"}},
		"cert only": {conf("listen: [{address: '::', port: 80, tls: {cert_file: c}}]"),
			[]string{"cert_file and key_file must both be set", "(file a.yaml)"}},
		"key only": {conf("listen: [{address: '::', port: 80, tls: {key_file: k}}]"),
			[]string{"both be set"}},
		"empty listen": {conf("listen: []"), []string{"listen: at least one entry"}},
		"no address":   {conf("listen: [{port: 80}]"), []string{"listen[0].address"}},
		"bad port":     {conf("listen: [{address: '::', port: 0}]"), []string{"listen[0].port"}},
		"dup listener": {conf("listen: [{address: '::', port: 1}, {address: '::', port: 1}]"),
			[]string{"already listed"}},
		"url ok":          {conf("cluster: {url: 'https://example.com:8443/x/'}"), nil},
		"url relative":    {conf("cluster: {url: /x}"), []string{"cluster.url", "(file a.yaml)"}},
		"url ftp":         {conf("cluster: {url: 'ftp://h'}"), []string{"cluster.url"}},
		"url no host":     {conf("cluster: {url: 'http://'}"), []string{"cluster.url"}},
		"url query":       {conf("cluster: {url: 'http://h/?a=b'}"), []string{"cluster.url"}},
		"url fragment":    {conf("cluster: {url: 'http://h/#f'}"), []string{"cluster.url"}},
		"url empty query": {conf("cluster: {url: 'http://h/?'}"), []string{"cluster.url"}},
		"url unparsable":  {conf("cluster: {url: 'http://h:x'}"), []string{"cluster.url"}},
		"node bad": {conf("cluster: {node: 'Bad Node'}"),
			[]string{"cluster.node", "(file a.yaml)"}},
		"node good": {conf("cluster: {node: n-1.x}"), nil},
		"base path": {conf("base_path: api"), []string{"base_path", "(file a.yaml)"}},
		"base root": {conf("base_path: /"), nil},
		"proxy bad": {conf("trusted_proxies: [nope]"),
			[]string{"trusted_proxies[0]", "(file a.yaml)"}},
		"ttl short":   {conf("auth: {token_ttl: 1s}"), []string{"auth.token_ttl", "(file a.yaml)"}},
		"rate zero":   {conf("auth: {login_rate: 0/m}"), []string{"auth.login_rate"}},
		"mtime short": {conf("mtime_precision: unlimited"), []string{"mtime_precision"}},
		"log level":   {conf("log: {level: loud}"), []string{"log.level", "(file a.yaml)"}},
		"log format":  {conf("log: {format: xml}"), []string{"log.format"}},
		"later file named": {files{"conf.d/a.yaml": "log: {level: loud}",
			"conf.d/b.yaml": "log: {level: bogus}"},
			[]string{"(file b.yaml)"}},
		"all together": {conf("base_path: x\nlog: {level: loud}"),
			[]string{"base_path", "log.level"}},
		"share ok": {files{"shares.d/media.yaml": "path: /srv/media\nexclude: ['*.tmp']\nmin_age: 5m\n" +
			"follow_symlinks: true"}, nil},
		"share name": {files{"shares.d/Bad Name.yaml": "path: /a"},
			[]string{"shares.Bad Name", "(file Bad Name.yaml)"}},
		"share rel": {share("path: rel/dir"),
			[]string{"shares.m.path", "must be absolute", "(file m.yaml)"}},
		"share none":   {share("exclude: []"), []string{"shares.m.path: must not be empty"}},
		"share glob":   {share("path: /a\nexclude: ['[']"), []string{"shares.m.exclude[0]"}},
		"share slash":  {share("path: /a\nexclude: [a/b]"), []string{"must not contain"}},
		"share minage": {share("path: /a\nmin_age: unlimited"), []string{"shares.m.min_age"}},
		"share empty":  {share(""), []string{"shares.m.path: must not be empty"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := load(t, tt.f)
			if len(tt.want) == 0 {
				if err != nil || got == nil {
					t.Fatalf("got %+v, %v", got, err)
				}
				return
			}
			if err == nil || got != nil {
				t.Fatalf("got %+v, %v", got, err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestLoadDirValues(t *testing.T) {
	t.Parallel()
	got, err := load(t, files{
		"conf.d/a.yaml": "base_path: /\ncluster: {url: 'http://h/'}\ntrusted_proxies: [192.0.2.1]\n" +
			"listen:\n  - address: 192.0.2.1\n    port: 1\n" +
			"    tls: {cert_file: c.pem, key_file: /abs/k.pem}\n",
		"shares.d/m.yaml": "path: /srv/m\nmin_age: 1h\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.BasePath != "" || got.Cluster.URL != "http://h" {
		t.Errorf("normalized values: %q %q", got.BasePath, got.Cluster.URL)
	}
	wantProxies := []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	if !reflect.DeepEqual(got.TrustedProxies, wantProxies) {
		t.Errorf("proxies %v", got.TrustedProxies)
	}
	m := got.Shares["m"]
	wantShare := Share{Path: "/srv/m", Exclude: []string{".*"}, MinAge: units.Duration(time.Hour)}
	if !reflect.DeepEqual(m, wantShare) {
		t.Errorf("share %+v", m)
	}
	wantSources := map[string]string{
		"base_path": "a.yaml", "cluster.url": "a.yaml", "trusted_proxies": "a.yaml", "listen": "a.yaml",
		"shares.m.path": "m.yaml", "shares.m.min_age": "m.yaml",
	}
	if !reflect.DeepEqual(got.Sources, wantSources) {
		t.Errorf("sources %v", got.Sources)
	}
}

func TestLoadDirResolvesTLSPaths(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, conf("listen:\n  - address: '::'\n    port: 1\n"+
		"    tls: {cert_file: tls/c.pem, key_file: /abs/../abs/k.pem}\n"))
	got, err := loadDir(dir, testHost)
	if err != nil {
		t.Fatal(err)
	}
	tls := got.Listen[0].TLS
	if tls.CertFile != filepath.Join(dir, "tls", "c.pem") || tls.KeyFile != "/abs/k.pem" {
		t.Errorf("tls %+v", tls)
	}
}

func TestLoadDirHostName(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, files{})
	_, err := loadDir(dir, failingHost)
	if err == nil || !strings.Contains(err.Error(), "cluster.node") {
		t.Errorf("host failure: %v", err)
	}
	// A configured node never asks for the host name.
	dir = writeDir(t, conf("cluster: {node: n}"))
	if got, err := loadDir(dir, failingHost); err != nil || got.Cluster.Node != "n" {
		t.Errorf("configured node: %+v, %v", got, err)
	}
	dir = writeDir(t, files{})
	empty := func() (string, error) { return "", nil }
	if _, err := loadDir(dir, empty); err == nil {
		t.Error("empty host name accepted")
	}
	// The exported function uses the real host name; the result is valid or names cluster.node.
	_, err = LoadDir(writeDir(t, files{}))
	if err != nil && !strings.Contains(err.Error(), "cluster.node") {
		t.Errorf("LoadDir: %v", err)
	}
}

func TestLoadDirUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	tests := map[string]string{
		"conf.d/a.yaml":   "settings file",
		"shares.d/m.yaml": "share file",
	}
	for rel, name := range tests {
		t.Run(name, func(t *testing.T) {
			dir := writeDir(t, files{rel: "path: /a\n"})
			file := filepath.Join(dir, rel)
			if err := os.Chmod(file, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDir(dir, testHost); err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("error %v does not name %s", err, file)
			}
		})
	}
	for _, sub := range []string{"conf.d", "shares.d"} {
		t.Run("dir "+sub, func(t *testing.T) {
			dir := writeDir(t, files{sub + "/a.yaml": "{}"})
			if err := os.Chmod(filepath.Join(dir, sub), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, sub), 0o700) })
			if _, err := loadDir(dir, testHost); err == nil || !strings.Contains(err.Error(), sub) {
				t.Errorf("error %v does not name %s", err, sub)
			}
		})
	}
}

func TestLoadDirNotADirectory(t *testing.T) {
	t.Parallel()
	if _, err := load(t, files{"conf.d": "file where a directory belongs"}); err == nil {
		t.Error("file named conf.d accepted")
	}
}

func TestAttribute(t *testing.T) {
	t.Parallel()
	sources := map[string]string{"listen": "a.yaml", "auth.token_ttl": "b.yaml"}
	tests := []struct{ msg, want string }{
		{"listen[2].tls: bad", "listen[2].tls: bad (file a.yaml)"},
		{"auth.token_ttl: bad", "auth.token_ttl: bad (file b.yaml)"},
		{"auth.login_rate: bad", "auth.login_rate: bad"},
		{"cluster: bad", "cluster: bad"},
		{"no key here", "no key here"},
	}
	for _, tt := range tests {
		if got := attribute(errors.New(tt.msg), sources).Error(); got != tt.want {
			t.Errorf("attribute(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}
