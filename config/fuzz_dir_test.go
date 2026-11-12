package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzLoadDir(f *testing.F) {
	dir := f.TempDir()
	for _, sub := range []string{confDirName, sharesDirName} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			f.Fatal(err)
		}
	}
	settingsFile := filepath.Join(dir, confDirName, "a.yaml")
	shareFile := filepath.Join(dir, sharesDirName, "m.yaml")
	settingsSeeds := []string{
		"", "{}", "null", "[", "\x00", "---\n", "a: 1\n---\nb: 2\n", "x: &a [*a]\n", "listen: 1\n",
		"listen: []\n", "base_path: api\n", "base_path: /\n", "log: {level: loud}\n",
		"cluster: {node: Bad Node}\n", "cluster: {url: 'http://h/'}\n", "cluster: {url: 'http://h/?q'}\n",
		"listen: [{address: '::', port: 1, plain: true, tls: {cert_file: c, key_file: k}}]\n",
		"listen: [{address: '::', port: 1, tls: {cert_file: c}}]\n",
		"trusted_proxies: [10.0.0.0/8, nope]\n",
		"auth: {token_ttl: 1h, login_rate: 5/s}\n", "mtime_precision: unlimited\n", "shares: {}\n",
	}
	shareSeeds := []string{
		"path: /srv/m\n", "path: rel\n", "", "exclude: ['[']\npath: /a\n", "path: /a\nmin_age: 1h\n",
		"path: /a\nname: x\n", "path: /a\n---\npath: /b\n", "path: /a\nexclude: [a/b]\n", "\x00",
	}
	for _, s := range settingsSeeds {
		f.Add([]byte(s), []byte(shareSeeds[0]))
	}
	for _, s := range shareSeeds {
		f.Add([]byte(settingsSeeds[1]), []byte(s))
	}
	f.Fuzz(func(t *testing.T, settings, share []byte) {
		if err := os.WriteFile(settingsFile, settings, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shareFile, share, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadDir(dir, testHost)
		if err != nil {
			// A refused configuration yields nothing, so a caller cannot run half of one.
			if got != nil {
				t.Fatalf("loadDir failed with %v but returned settings", err)
			}
			return
		}
		checkSettingsInvariants(t, got)
	})
}

// checkSettingsInvariants holds what the rest of the server relies on after a successful load.
func checkSettingsInvariants(t *testing.T, s *Settings) {
	t.Helper()
	if len(s.Listen) == 0 {
		t.Fatal("accepted without a listener")
	}
	for _, l := range s.Listen {
		if !l.Address.IsValid() || l.Port < 1 || l.Port > 65535 || (l.Plain && l.TLS != nil) {
			t.Fatalf("accepted listener %+v", l)
		}
		if l.TLS != nil && (l.TLS.CertFile == "") != (l.TLS.KeyFile == "") {
			t.Fatalf("accepted half a TLS pair %+v", l.TLS)
		}
		if l.TLS != nil && l.TLS.CertFile != "" && !filepath.IsAbs(l.TLS.CertFile) {
			t.Fatalf("left a TLS path relative: %+v", l.TLS)
		}
	}
	if b := s.BasePath; b != "" && (!strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/")) {
		t.Fatalf("accepted base path %q", b)
	}
	if !namePattern.MatchString(s.Cluster.Node) || s.Cluster.Name == "" {
		t.Fatalf("accepted cluster %+v", s.Cluster)
	}
	if u := s.Cluster.URL; u != "" {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || strings.HasSuffix(u, "/") || strings.ContainsAny(u, "?#") {
			t.Fatalf("accepted cluster url %q", u)
		}
	}
	ttl := s.Auth.TokenTTL
	if ttl.Unlimited() || ttl.Std() < minTokenTTL || s.Auth.LoginRate.Count < 1 {
		t.Fatalf("accepted auth %+v", s.Auth)
	}
	for name, sh := range s.Shares {
		if !namePattern.MatchString(name) || !filepath.IsAbs(sh.Path) {
			t.Fatalf("accepted share %q at %q", name, sh.Path)
		}
	}
}
