package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

const (
	confDirName   = "conf.d"
	sharesDirName = "shares.d"
)

// LoadDir reads the settings files conf.d/*.yaml and the share files shares.d/*.yaml of
// configDir, merges the settings in file name order, applies defaults, resolves relative
// TLS paths against configDir and validates the result. Missing directories are empty
// ones, so an empty configDir gives the defaults. When the configuration is invalid the
// error lists every problem found and names the file a bad value came from.
func LoadDir(configDir string) (*Config, error) {
	return loadDir(configDir, os.Hostname)
}

// loadDir is LoadDir with the host name lookup passed in, so tests need not depend on the
// machine they run on.
func loadDir(configDir string, hostname func() (string, error)) (*Config, error) {
	sources := make(map[string]string)
	raw, problems := readSettings(filepath.Join(configDir, confDirName), sources)
	shares, shareProblems := readShares(filepath.Join(configDir, sharesDirName), sources)
	problems = append(problems, shareProblems...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("read server config %s: %w", configDir, errors.Join(problems...))
	}
	settings := &raw.Config
	settings.Shares = shares
	settings.Sources = sources
	problems = finishSettings(settings, raw.TrustedProxies, configDir, hostname)
	problems = append(problems, validateShareFiles(shares)...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid server config %s: %w", configDir, errors.Join(problems...))
	}
	return settings, nil
}

// readSettings decodes the files of dir in order onto one value and records, for every
// value a file sets, which file it was.
func readSettings(dir string, sources map[string]string) (*rawConfig, []error) {
	raw := &rawConfig{}
	names, err := yamlFiles(dir)
	if err != nil {
		return raw, []error{fmt.Errorf("%s: %w", dir, err)}
	}
	var problems []error
	for _, name := range names {
		keys, err := decodeFile(filepath.Join(dir, name), raw)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, key := range keys {
			sources[key] = name
		}
	}
	return raw, problems
}

// finishSettings applies defaults, normalizes and validates the merged settings. A problem
// names the file of its value when a file set it.
func finishSettings(
	s *Config, proxies []string, configDir string, hostname func() (string, error),
) []error {
	var problems []error
	s.TrustedProxies, problems = parseProxies(proxies)
	problems = append(problems, applyDirDefaults(s, hostname)...)
	problems = append(problems, normalizeSettings(s)...)
	for i := range s.Listen {
		resolveTLS(&s.Listen[i].Listener, configDir)
	}
	problems = append(problems, validateSettings(s)...)
	for i, p := range problems {
		problems[i] = attribute(p, s.Sources)
	}
	return problems
}

// applyDirDefaults fills in the settings keys that no file set.
func applyDirDefaults(s *Config, hostname func() (string, error)) []error {
	if s.Listen == nil {
		s.Listen = []Endpoint{{Listener: Listener{Address: netip.IPv6Unspecified(), Port: defaultPort}}}
	}
	if s.BasePath == "" {
		s.BasePath = defaultBasePath
	}
	if s.Cluster.Name == "" {
		s.Cluster.Name = defaultClusterName
	}
	applyLimitDefaults(&s.Auth.TokenTTL, &s.Auth.LoginRate, &s.MTimePrecision)
	applyLogDefaults(&s.Log)
	if s.Cluster.Node != "" {
		return nil
	}
	host, err := hostname()
	if err != nil {
		return []error{fmt.Errorf("cluster.node: host name unavailable, set it in a file: %w", err)}
	}
	// Node names are lower case, but host names often are not.
	s.Cluster.Node = strings.ToLower(host)
	return nil
}

// normalizeSettings puts base_path and cluster.url into their canonical form.
func normalizeSettings(s *Config) []error {
	var problems []error
	base, err := normalizeBasePath(s.BasePath)
	if err != nil {
		problems = append(problems, err)
	} else {
		s.BasePath = base
	}
	u, err := normalizeURL(s.Cluster.URL)
	if err != nil {
		problems = append(problems, err)
	} else {
		s.Cluster.URL = u
	}
	return problems
}
