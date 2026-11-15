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
	return loadDirWithHostname(configDir, os.Hostname)
}

// loadDirWithHostname is LoadDir with the host name lookup passed in, so tests need not
// depend on the machine they run on.
func loadDirWithHostname(configDir string, hostname func() (string, error)) (*Config, error) {
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
	settings *Config, proxies []string, configDir string, hostname func() (string, error),
) []error {
	prefixes, problems := parseProxies(proxies)
	settings.TrustedProxies = prefixes
	problems = append(problems, applyDirDefaults(settings, hostname)...)
	problems = append(problems, normalizeSettings(settings)...)
	for i := range settings.Listen {
		resolveTLS(&settings.Listen[i].Listener, configDir)
	}
	problems = append(problems, validateSettings(settings)...)
	for i, problem := range problems {
		problems[i] = attribute(problem, settings.Sources)
	}
	return problems
}

// applyDirDefaults fills in the settings keys that no file set.
func applyDirDefaults(settings *Config, hostname func() (string, error)) []error {
	if settings.Listen == nil {
		settings.Listen = []Endpoint{{Listener: Listener{Address: netip.IPv6Unspecified(), Port: defaultPort}}}
	}
	if settings.BasePath == "" {
		settings.BasePath = defaultBasePath
	}
	if settings.Cluster.Name == "" {
		settings.Cluster.Name = defaultClusterName
	}
	applyLimitDefaults(&settings.Auth.TokenTTL, &settings.Auth.LoginRate, &settings.MTimePrecision)
	applyLogDefaults(&settings.Log)
	if settings.Cluster.Node != "" {
		return nil
	}
	host, err := hostname()
	if err != nil {
		return []error{fmt.Errorf("cluster.node: host name unavailable, set it in a file: %w", err)}
	}
	settings.Cluster.Node = nodeNameOf(host)
	return nil
}

// maxNameLength is the longest name namePattern accepts.
const maxNameLength = 64

// nodeNameOf turns a host name into a valid node name. A server with nothing configured
// must start on any machine, and host names are often upper case, longer than a node name
// may be, or hold characters a node name may not. An empty result is left for validation
// to report.
func nodeNameOf(host string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, host)
	// A name starts with a letter or a digit.
	name = strings.TrimLeft(name, "._-")
	if len(name) > maxNameLength {
		name = name[:maxNameLength]
	}
	return name
}

// normalizeSettings puts base_path and cluster.url into their canonical form.
func normalizeSettings(settings *Config) []error {
	var problems []error
	base, err := normalizeBasePath(settings.BasePath)
	if err != nil {
		problems = append(problems, err)
	} else {
		settings.BasePath = base
	}
	clusterURL, err := normalizeURL(settings.Cluster.URL)
	if err != nil {
		problems = append(problems, err)
	} else {
		settings.Cluster.URL = clusterURL
	}
	return problems
}
