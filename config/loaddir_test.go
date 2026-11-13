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

func load(t *testing.T, f files) (*Config, error) {
	t.Helper()
	return loadDirWithHostname(writeDir(t, f), testHost)
}

func defaultSettings() *Config {
	return &Config{
		Listen:   []Endpoint{{Listener: Listener{Address: netip.IPv6Unspecified(), Port: 8480}}},
		BasePath: "/api",
		Cluster:  Cluster{Name: "default", Node: "test-host"},
		Auth: Auth{
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
	want.Cluster = Cluster{Name: "x", Node: "n1"}
	want.Auth = Auth{
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

// loadError loads f and fails the test unless the configuration is refused. It returns the
// error.
func loadError(t *testing.T, f files) error {
	t.Helper()
	got, err := load(t, f)
	if err == nil || got != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	return err
}

// requireLoadError fails the test unless loading f is refused with an error that holds every
// text in want.
func requireLoadError(t *testing.T, f files, want []string) {
	t.Helper()
	err := loadError(t, f)
	for _, text := range want {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("error %q lacks %q", err, text)
		}
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
		"both dirs": {
			files{"conf.d/a.yaml": "x: 1", "shares.d/m.yaml": "y: 1"},
			[]string{"a.yaml", "m.yaml"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireLoadError(t, tt.f, tt.want)
		})
	}
}

func TestLoadDirAccepts(t *testing.T) {
	t.Parallel()
	tests := map[string]files{
		"plain":     conf("listen: [{address: '::', port: 80, plain: true}]"),
		"tls":       conf("listen: [{address: '::', port: 80, tls: {cert_file: c, key_file: k}}]"),
		"url ok":    conf("cluster: {url: 'https://example.com:8443/x/'}"),
		"node good": conf("cluster: {node: n-1.x}"),
		"base root": conf("base_path: /"),
		"share ok": {"shares.d/media.yaml": "path: /srv/media\nexclude: ['*.tmp']\nmin_age: 5m\n" +
			"follow_symlinks: true"},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := load(t, f)
			if err != nil || got == nil {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestLoadDirRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		f    files
		want []string
	}{
		"plain and tls": {
			conf("listen: [{address: '::', port: 80, plain: true,\n tls: {cert_file: c, key_file: k}}]"),
			[]string{"listen[0].plain: must not be true together with tls", "(file a.yaml)"},
		},
		"cert only": {
			conf("listen: [{address: '::', port: 80, tls: {cert_file: c}}]"),
			[]string{"cert_file and key_file must both be set", "(file a.yaml)"},
		},
		"key only":     {conf("listen: [{address: '::', port: 80, tls: {key_file: k}}]"), []string{"both be set"}},
		"empty listen": {conf("listen: []"), []string{"listen: at least one entry"}},
		"no address":   {conf("listen: [{port: 80}]"), []string{"listen[0].address"}},
		"bad port":     {conf("listen: [{address: '::', port: 0}]"), []string{"listen[0].port"}},
		"dup listener": {
			conf("listen: [{address: '::', port: 1}, {address: '::', port: 1}]"),
			[]string{"already listed"},
		},
		"url relative":    {conf("cluster: {url: /x}"), []string{"cluster.url", "(file a.yaml)"}},
		"url ftp":         {conf("cluster: {url: 'ftp://h'}"), []string{"cluster.url"}},
		"url no host":     {conf("cluster: {url: 'http://'}"), []string{"cluster.url"}},
		"url query":       {conf("cluster: {url: 'http://h/?a=b'}"), []string{"cluster.url"}},
		"url fragment":    {conf("cluster: {url: 'http://h/#f'}"), []string{"cluster.url"}},
		"url empty query": {conf("cluster: {url: 'http://h/?'}"), []string{"cluster.url"}},
		"url unparsable":  {conf("cluster: {url: 'http://h:x'}"), []string{"cluster.url"}},
		"node bad":        {conf("cluster: {node: 'Bad Node'}"), []string{"cluster.node", "(file a.yaml)"}},
		"base path":       {conf("base_path: api"), []string{"base_path", "(file a.yaml)"}},
		"proxy bad":       {conf("trusted_proxies: [nope]"), []string{"trusted_proxies[0]", "(file a.yaml)"}},
		"ttl short":       {conf("auth: {token_ttl: 1s}"), []string{"auth.token_ttl", "(file a.yaml)"}},
		"rate zero":       {conf("auth: {login_rate: 0/m}"), []string{"auth.login_rate"}},
		"mtime short":     {conf("mtime_precision: unlimited"), []string{"mtime_precision"}},
		"log level":       {conf("log: {level: loud}"), []string{"log.level", "(file a.yaml)"}},
		"log format":      {conf("log: {format: xml}"), []string{"log.format"}},
		"later file named": {
			files{"conf.d/a.yaml": "log: {level: loud}", "conf.d/b.yaml": "log: {level: bogus}"},
			[]string{"(file b.yaml)"},
		},
		"all together": {
			conf("base_path: x\nlog: {level: loud}"),
			[]string{"base_path", "log.level"},
		},
		"share name": {
			files{"shares.d/Bad Name.yaml": "path: /a"},
			[]string{"shares.Bad Name", "(file Bad Name.yaml)"},
		},
		"share rel": {
			share("path: rel/dir"),
			[]string{"shares.m.path", "must be absolute", "(file m.yaml)"},
		},
		"share none":   {share("exclude: []"), []string{"shares.m.path: must not be empty"}},
		"share glob":   {share("path: /a\nexclude: ['[']"), []string{"shares.m.exclude[0]"}},
		"share slash":  {share("path: /a\nexclude: [a/b]"), []string{"must not contain"}},
		"share minage": {share("path: /a\nmin_age: unlimited"), []string{"shares.m.min_age"}},
		"share empty":  {share(""), []string{"shares.m.path: must not be empty"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireLoadError(t, tt.f, tt.want)
		})
	}
}

// TestLoadDirReasons checks that a caller can tell why a configuration was refused without
// reading the message.
func TestLoadDirReasons(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		f    files
		want error
	}{
		"rate":          {conf("auth: {login_rate: 5/d}"), ErrInvalidRate},
		"two documents": {conf("a: 1\n---\nb: 2\n"), ErrMultipleDocuments},
		"base path":     {conf("base_path: api"), ErrBasePathNoSlash},
		"empty path":    {share("exclude: []"), ErrEmpty},
		"empty address": {conf("listen: [{port: 80}]"), ErrEmpty},
		"not one of":    {conf("log: {level: loud}"), ErrNotOneOf},
		"short ttl":     {conf("auth: {token_ttl: 1s}"), ErrDurationTooShort},
		"short min age": {share("path: /a\nmin_age: unlimited"), ErrDurationTooShort},
		"no listener":   {conf("listen: []"), ErrNoListener},
		"port":          {conf("listen: [{address: '::', port: 0}]"), ErrPortOutOfRange},
		"duplicate":     {conf("listen: [{address: '::', port: 1}, {address: '::', port: 1}]"), ErrDuplicateListener},
		"half tls":      {conf("listen: [{address: '::', port: 80, tls: {cert_file: c}}]"), ErrTLSPairIncomplete},
		"plain and tls": {
			conf("listen: [{address: '::', port: 80, plain: true, tls: {cert_file: c, key_file: k}}]"),
			ErrPlainWithTLS,
		},
		"node name":      {conf("cluster: {node: 'Bad Node'}"), ErrNamePattern},
		"share name":     {files{"shares.d/Bad Name.yaml": "path: /a"}, ErrNamePattern},
		"exclude slash":  {share("path: /a\nexclude: [a/b]"), ErrExcludeSlash},
		"rate count":     {conf("auth: {login_rate: 0/m}"), ErrCountBelowOne},
		"cluster url":    {conf("cluster: {url: ftp://h}"), ErrURLInvalid},
		"relative share": {share("path: rel/dir"), ErrPathNotAbsolute},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := loadError(t, tt.f); !errors.Is(err, tt.want) {
				t.Errorf("error %q is not %q", err, tt.want)
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
	got, err := loadDirWithHostname(dir, testHost)
	if err != nil {
		t.Fatal(err)
	}
	tls := got.Listen[0].TLS
	if tls.CertFile != filepath.Join(dir, "tls", "c.pem") || tls.KeyFile != "/abs/k.pem" {
		t.Errorf("tls %+v", tls)
	}
}

func TestLoadDirHostNameUnavailable(t *testing.T) {
	t.Parallel()
	_, err := loadDirWithHostname(writeDir(t, files{}), failingHost)
	if !errors.Is(err, errNoHost) || !strings.Contains(err.Error(), "cluster.node") {
		t.Errorf("host failure: %v", err)
	}
}

func TestLoadDirConfiguredNodeSkipsHostName(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, conf("cluster: {node: n}"))
	if got, err := loadDirWithHostname(dir, failingHost); err != nil || got.Cluster.Node != "n" {
		t.Errorf("configured node: %+v, %v", got, err)
	}
}

func TestLoadDirEmptyHostName(t *testing.T) {
	t.Parallel()
	emptyHost := func() (string, error) { return "", nil }
	if _, err := loadDirWithHostname(writeDir(t, files{}), emptyHost); err == nil {
		t.Error("empty host name accepted")
	}
}

func TestLoadDirRealHostName(t *testing.T) {
	t.Parallel()
	// The exported function uses the real host name; the result is valid or names cluster.node.
	_, err := LoadDir(writeDir(t, files{}))
	if err != nil && !strings.Contains(err.Error(), "cluster.node") {
		t.Errorf("LoadDir: %v", err)
	}
}

func TestNodeNameOf(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 70)
	tests := []struct{ host, want string }{
		{"east", "east"},
		{"East.Example.COM", "east.example.com"},
		{"build host_1", "build-host_1"},
		{"h\u00f6st", "h-st"},
		{"-.lead", "lead"},
		{long + ".local", long[:64]},
		{"", ""},
		{"...", ""},
	}
	for _, tt := range tests {
		got := nodeNameOf(tt.host)
		if got != tt.want {
			t.Errorf("nodeNameOf(%q) = %q, want %q", tt.host, got, tt.want)
		}
		// Whatever the host is called, the name is valid or empty; empty is reported.
		if got != "" && !namePattern.MatchString(got) {
			t.Errorf("nodeNameOf(%q) = %q is not a valid name", tt.host, got)
		}
	}
}

func TestLoadDirUnreadable(t *testing.T) {
	t.Parallel()
	// A link to nothing is listed like a file and cannot be read. Taking away read
	// permission would not stop root, which runs the tests on Linux.
	tests := map[string]string{
		"conf.d/a.yaml":   "settings file",
		"shares.d/m.yaml": "share file",
	}
	for rel, name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := writeDir(t, files{filepath.Join(filepath.Dir(rel), "other.txt"): ""})
			file := filepath.Join(dir, rel)
			if err := os.Symlink(filepath.Join(dir, "nowhere"), file); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDirWithHostname(dir, testHost); err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("error %v does not name %s", err, file)
			}
		})
	}
	// A file where a folder belongs cannot be listed, by any user.
	for _, sub := range []string{"conf.d", "shares.d"} {
		t.Run("dir "+sub, func(t *testing.T) {
			t.Parallel()
			dir := writeDir(t, files{sub: "a file where a folder belongs"})
			if _, err := loadDirWithHostname(dir, testHost); err == nil || !strings.Contains(err.Error(), sub) {
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

func TestShareNamesAreSorted(t *testing.T) {
	t.Parallel()
	got, err := load(t, files{
		"shares.d/b.yaml": "path: /srv/b\n",
		"shares.d/a.yaml": "path: /srv/a\n",
		"shares.d/c.yaml": "path: /srv/c\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Join(got.ShareNames(), ","); names != "a,b,c" {
		t.Errorf("ShareNames = %s", names)
	}
}
