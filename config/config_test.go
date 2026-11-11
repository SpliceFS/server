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

	"github.com/BlueRival/splicefs/internal/units"
)

const validYAML = `listen:
  - address: 192.0.2.10
    port: 8480
base_path: /api
trusted_proxies: [192.0.2.1]
shares:
  media:
    path: /srv/media
cluster:
  name: example
  node: east
  nodes_file: /srv/shared/nodes.yaml
auth:
  users_dir: /srv/shared/users
  keys_file: /srv/shared/keys.yaml
`

// writeConfig writes content as config.yaml in a new directory and returns it.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mutate replaces old with new in validYAML and fails if old is not there.
func mutate(t *testing.T, old, replacement string) string {
	t.Helper()
	if !strings.Contains(validYAML, old) {
		t.Fatalf("validYAML does not contain %q", old)
	}
	return strings.Replace(validYAML, old, replacement, 1)
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, strings.Replace(validYAML, "base_path: /api\n", "", 1))
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &Config{
		Listen:         []Listener{{Address: netip.MustParseAddr("192.0.2.10"), Port: 8480}},
		BasePath:       "/api",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")},
		Shares:         map[string]Share{"media": {Path: "/srv/media", Exclude: []string{".*"}}},
		Cluster:        Cluster{Name: "example", Node: "east", NodesFile: "/srv/shared/nodes.yaml"},
		Auth: Auth{
			UsersDir:  "/srv/shared/users",
			KeysFile:  "/srv/shared/keys.yaml",
			TokenTTL:  units.Duration(30 * time.Minute),
			LoginRate: Rate{Count: 10, Per: time.Minute},
		},
		MTimePrecision: units.Duration(time.Second),
		Log:            Log{Level: "info", Format: "text"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadFull(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, `listen:
  - address: 192.0.2.10
    port: 8480
    tls:
      cert_file: tls/fullchain.pem
      key_file: /etc/tls/privkey.pem
  - address: "2001:db8::1"
    port: 8481
base_path: /files/
trusted_proxies: [192.0.2.1, 198.51.100.0/24, "2001:db8::2", "2001:db8:1::/48"]
shares:
  media:
    path: media/../media
    exclude: []
    min_age: 5m
    follow_symlinks: true
  docs.v2:
    path: /srv/docs
    exclude: ["*.tmp", "[a-c]x"]
    min_age: 0
cluster:
  name: example
  node: east
  nodes_file: nodes.yaml
auth:
  users_dir: ./users
  keys_file: /srv/keys.yaml
  token_ttl: 2h
  login_rate: 5/s
mtime_precision: 2s
log:
  level: debug
  format: json
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &Config{
		Listen: []Listener{
			{
				Address: netip.MustParseAddr("192.0.2.10"),
				Port:    8480,
				TLS:     &TLS{CertFile: filepath.Join(dir, "tls/fullchain.pem"), KeyFile: "/etc/tls/privkey.pem"},
			},
			{Address: netip.MustParseAddr("2001:db8::1"), Port: 8481},
		},
		BasePath: "/files",
		TrustedProxies: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.1/32"),
			netip.MustParsePrefix("198.51.100.0/24"),
			netip.MustParsePrefix("2001:db8::2/128"),
			netip.MustParsePrefix("2001:db8:1::/48"),
		},
		Shares: map[string]Share{
			"media":   {Path: filepath.Join(dir, "media"), Exclude: []string{}, MinAge: units.Duration(5 * time.Minute), FollowSymlinks: true},
			"docs.v2": {Path: "/srv/docs", Exclude: []string{"*.tmp", "[a-c]x"}},
		},
		Cluster: Cluster{Name: "example", Node: "east", NodesFile: filepath.Join(dir, "nodes.yaml")},
		Auth: Auth{
			UsersDir:  filepath.Join(dir, "users"),
			KeysFile:  "/srv/keys.yaml",
			TokenTTL:  units.Duration(2 * time.Hour),
			LoginRate: Rate{Count: 5, Per: time.Second},
		},
		MTimePrecision: units.Duration(2 * time.Second),
		Log:            Log{Level: "debug", Format: "json"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
	if got := cfg.Shares["media"].Exclude; got == nil || len(got) != 0 {
		t.Errorf("explicit empty exclude = %#v, want empty non-nil", got)
	}
	if got, want := cfg.ShareNames(), []string{"docs.v2", "media"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ShareNames = %v, want %v", got, want)
	}
}

func TestLoadBasePath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/", ""},
		{"/api", "/api"},
		{"/api/", "/api"},
		{"/a/b/", "/a/b"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeConfig(t, mutate(t, "base_path: /api", "base_path: "+tc.in)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BasePath != tc.want {
				t.Errorf("BasePath = %q, want %q", cfg.BasePath, tc.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	const noCluster = "listen:\n  - address: 192.0.2.10\n    port: 8480\nshares:\n  media:\n    path: /srv/media\n"
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"unknown key", mutate(t, "base_path", "base_pat"), []string{"base_pat"}},
		{"unknown nested key", mutate(t, "port: 8480", "port: 8480\n    bogus: 1"), []string{"bogus"}},
		{"syntax error", "listen: [", []string{"parse"}},
		{"empty file", "", []string{"file is empty"}},
		{"only a comment", "# nothing\n", []string{"file is empty"}},
		{"two documents", validYAML + "---\n" + validYAML, []string{"more than one YAML document"}},
		{"broken second document", validYAML + "---\nlisten: [\n", []string{"parse"}},
		{"unparsable address", mutate(t, "192.0.2.10", "nope"), []string{"nope"}},
		{"unparsable duration", mutate(t, "path: /srv/media\n", "path: /srv/media\n    min_age: 1h30m\n"), []string{"invalid duration"}},
		{"unparsable rate", mutate(t, "keys.yaml\n", "keys.yaml\n  login_rate: fast\n"), []string{"invalid rate"}},
		{"no listen", mutate(t, "listen:\n  - address: 192.0.2.10\n    port: 8480\n", ""), []string{"listen: at least one entry"}},
		{"missing address", mutate(t, "address: 192.0.2.10\n    port: 8480", "port: 8480"), []string{"listen[0].address: must not be empty"}},
		{"port zero", mutate(t, "port: 8480", "port: 0"), []string{"listen[0].port: 0 is not between 1 and 65535"}},
		{"port too high", mutate(t, "port: 8480", "port: 65536"), []string{"listen[0].port: 65536 is not between 1 and 65535"}},
		{
			"duplicate listener",
			mutate(t, "port: 8480", "port: 8480\n  - address: 192.0.2.10\n    port: 8480"),
			[]string{"listen[1]: 192.0.2.10:8480 is already listed in listen[0]"},
		},
		{"tls cert only", mutate(t, "port: 8480", "port: 8480\n    tls:\n      cert_file: a.pem"), []string{"listen[0].tls: cert_file and key_file"}},
		{"tls key only", mutate(t, "port: 8480", "port: 8480\n    tls:\n      key_file: a.pem"), []string{"listen[0].tls: cert_file and key_file"}},
		{
			"bad proxies",
			mutate(t, "[192.0.2.1]", "[192.0.2.1, nope, 10.0.0.0/33]"),
			[]string{"trusted_proxies[1]", "trusted_proxies[2]"},
		},
		{"base path without slash", mutate(t, "base_path: /api", "base_path: api"), []string{`base_path: "api" must start with "/"`}},
		{"no shares", mutate(t, "shares:\n  media:\n    path: /srv/media\n", "shares: {}\n"), []string{"shares: at least one share"}},
		{"bad share name", mutate(t, "  media:", "  Media:"), []string{"shares.Media: name must match"}},
		{"empty share path", mutate(t, "path: /srv/media", "path: ''"), []string{"shares.media.path: must not be empty"}},
		{
			"bad excludes",
			mutate(t, "path: /srv/media\n", "path: /srv/media\n    exclude: [\"a/b\", \"[\"]\n"),
			[]string{"shares.media.exclude[0]", "must not contain", "shares.media.exclude[1]", "syntax error in pattern"},
		},
		{"unlimited min_age", mutate(t, "path: /srv/media\n", "path: /srv/media\n    min_age: unlimited\n"), []string{"shares.media.min_age: unlimited is not allowed"}},
		{
			"missing cluster and auth",
			noCluster,
			[]string{
				"cluster.name: must not be empty", "cluster.node: must not be empty", "cluster.nodes_file: must not be empty",
				"auth.users_dir: must not be empty", "auth.keys_file: must not be empty",
			},
		},
		{"short token ttl", mutate(t, "keys.yaml\n", "keys.yaml\n  token_ttl: 30s\n"), []string{"auth.token_ttl: 30s is not allowed", "at least 1m"}},
		{"unlimited token ttl", mutate(t, "keys.yaml\n", "keys.yaml\n  token_ttl: unlimited\n"), []string{"auth.token_ttl: unlimited is not allowed"}},
		{"zero login rate", mutate(t, "keys.yaml\n", "keys.yaml\n  login_rate: 0/m\n"), []string{"auth.login_rate: count 0 is not at least 1"}},
		{"short mtime precision", validYAML + "mtime_precision: 0.5s\n", []string{"mtime_precision: 0.5s is not allowed"}},
		{"unlimited mtime precision", validYAML + "mtime_precision: unlimited\n", []string{"mtime_precision: unlimited is not allowed"}},
		{
			"bad log",
			validYAML + "log:\n  level: trace\n  format: xml\n",
			[]string{`log.level: "trace" is not one of debug, info, warn, error`, `log.format: "xml" is not one of text, json`},
		},
		{
			"every problem at once",
			mutate(t, "port: 8480", "port: 0") + "log:\n  level: trace\n",
			[]string{"listen[0].port", "log.level"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("Load succeeded: %+v", cfg)
			}
			if cfg != nil {
				t.Errorf("Load returned a config with an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load of an empty directory: %v, want os.ErrNotExist", err)
	}
}

func TestValidateShareNegativeMinAge(t *testing.T) {
	t.Parallel()
	errs := validateShare("media", Share{Path: "/srv/media", MinAge: units.Duration(-time.Second)})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "shares.media.min_age") {
		t.Errorf("validateShare = %v", errs)
	}
}

func TestRateUnmarshalText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Rate
	}{
		{"10/m", Rate{10, time.Minute}},
		{"1/s", Rate{1, time.Second}},
		{"0/h", Rate{0, time.Hour}},
		{"600/h", Rate{600, time.Hour}},
	}
	for _, tc := range tests {
		var got Rate
		if err := got.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("UnmarshalText(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("UnmarshalText(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestRateUnmarshalTextErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "10", "/m", "10/", "x/m", "-1/m", "+1/m", "1.5/m", "10/d", "10/min", "10/m/x", "99999999999/s"} {
		r := Rate{Count: 7, Per: time.Hour}
		err := r.UnmarshalText([]byte(in))
		if err == nil {
			t.Errorf("UnmarshalText(%q) succeeded with %+v", in, r)
			continue
		}
		if !strings.Contains(err.Error(), `"`+in+`"`) || !strings.Contains(err.Error(), "<count>/<s|m|h>") {
			t.Errorf("UnmarshalText(%q) error %q does not name the value and the form", in, err)
		}
		if r != (Rate{Count: 7, Per: time.Hour}) {
			t.Errorf("UnmarshalText(%q) changed the value to %+v", in, r)
		}
	}
}

func TestRateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   Rate
		want string
	}{
		{Rate{10, time.Minute}, "10/m"},
		{Rate{3, time.Second}, "3/s"},
		{Rate{60, time.Hour}, "60/h"},
		{Rate{5, 90 * time.Second}, "5/1m30s"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
