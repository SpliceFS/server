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
		got, err := loadDirWithHostname(dir, testHost)
		if err != nil {
			// A refused configuration yields nothing, so a caller cannot run half of one.
			if got != nil {
				t.Fatalf("loadDirWithHostname failed with %v but returned settings", err)
			}
			return
		}
		checkSettingsInvariants(t, got)
	})
}

// checkSettingsInvariants holds what the rest of the server relies on after a successful load.
func checkSettingsInvariants(t *testing.T, settings *Config) {
	t.Helper()
	if len(settings.Listen) == 0 {
		t.Fatal("accepted without a listener")
	}
	for _, endpoint := range settings.Listen {
		checkEndpointInvariants(t, endpoint)
	}
	if base := settings.BasePath; base != "" && (!strings.HasPrefix(base, "/") || strings.HasSuffix(base, "/")) {
		t.Fatalf("accepted base path %q", base)
	}
	checkClusterInvariants(t, settings.Cluster)
	checkAuthInvariants(t, settings.Auth)
	for name, share := range settings.Shares {
		if !namePattern.MatchString(name) || !filepath.IsAbs(share.Path) {
			t.Fatalf("accepted share %q at %q", name, share.Path)
		}
	}
}

// checkAuthInvariants holds what the auth keys must satisfy after a successful load.
func checkAuthInvariants(t *testing.T, auth Auth) {
	t.Helper()
	if auth.TokenTTL.Unlimited() || auth.TokenTTL.Std() < minTokenTTL || auth.LoginRate.Count < 1 {
		t.Fatalf("accepted auth %+v", auth)
	}
}

// checkEndpointInvariants holds what a listener must satisfy after a successful load.
func checkEndpointInvariants(t *testing.T, endpoint Endpoint) {
	t.Helper()
	if !endpoint.Address.IsValid() || endpoint.Port < minPort || endpoint.Port > maxPort ||
		(endpoint.Plain && endpoint.TLS != nil) {
		t.Fatalf("accepted listener %+v", endpoint)
	}
	if endpoint.TLS == nil {
		return
	}
	if (endpoint.TLS.CertFile == "") != (endpoint.TLS.KeyFile == "") {
		t.Fatalf("accepted half a TLS pair %+v", endpoint.TLS)
	}
	if endpoint.TLS.CertFile != "" && !filepath.IsAbs(endpoint.TLS.CertFile) {
		t.Fatalf("left a TLS path relative: %+v", endpoint.TLS)
	}
}

// checkClusterInvariants holds what the cluster keys must satisfy after a successful load.
func checkClusterInvariants(t *testing.T, cluster Cluster) {
	t.Helper()
	if !namePattern.MatchString(cluster.Node) || cluster.Name == "" {
		t.Fatalf("accepted cluster %+v", cluster)
	}
	if cluster.URL == "" {
		return
	}
	parsed, err := url.Parse(cluster.URL)
	if err != nil || parsed.Host == "" || strings.HasSuffix(cluster.URL, "/") ||
		strings.ContainsAny(cluster.URL, "?#") {
		t.Fatalf("accepted cluster url %q", cluster.URL)
	}
}
