// Package config loads and validates the configuration of the SpliceFS server,
// the file config.yaml described in docs/configuration.md.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/common/units"
)

const (
	// DefaultDir is the configuration directory used when none is given.
	DefaultDir = "/etc/splicefs/server"
	// FileName is the name of the main file inside the configuration directory.
	FileName = "config.yaml"
)

// Defaults applied to keys that are absent.
const (
	defaultBasePath       = "/api"
	defaultTokenTTL       = 30 * time.Minute
	defaultMTimePrecision = time.Second
	defaultLogLevel       = "info"
	defaultLogFormat      = "text"
	defaultExclude        = ".*"
)

// Lower limits for values that have one.
const (
	minTokenTTL       = time.Minute
	minMTimePrecision = time.Second
)

// namePattern is the form of a share name.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Config is the server configuration.
type Config struct {
	// Listen lists the addresses and ports to serve on.
	Listen []Listener `yaml:"listen"`
	// BasePath is the URL prefix of the API: "" for the root, otherwise a path
	// that starts with a slash and does not end with one.
	BasePath string `yaml:"base_path"`
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed. The YAML key trusted_proxies is read by Load, which accepts
	// single addresses as well as prefixes.
	TrustedProxies []netip.Prefix `yaml:"-"`
	// Shares maps a share name to its settings.
	Shares map[string]Share `yaml:"shares"`
	// Cluster identifies this node and its cluster.
	Cluster Cluster `yaml:"cluster"`
	// Auth configures logins and tokens.
	Auth Auth `yaml:"auth"`
	// MTimePrecision is the precision at which modification times are compared.
	MTimePrecision units.Duration `yaml:"mtime_precision"`
	// Log configures logging.
	Log Log `yaml:"log"`
}

// Listener is one address and port to serve on.
type Listener struct {
	Address netip.Addr `yaml:"address"`
	Port    int        `yaml:"port"`
	// TLS is nil when a reverse proxy terminates TLS.
	TLS *TLS `yaml:"tls"`
}

// TLS names the certificate chain and private key of a listener.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Share is one shared directory.
type Share struct {
	// Path is the directory that is shared.
	Path string `yaml:"path"`
	// Exclude lists glob patterns for names that are never listed or served.
	Exclude []string `yaml:"exclude"`
	// MinAge hides files modified more recently than this.
	MinAge units.Duration `yaml:"min_age"`
	// FollowSymlinks allows links that stay inside the share to be followed.
	FollowSymlinks bool `yaml:"follow_symlinks"`
}

// Cluster names the cluster and this node.
type Cluster struct {
	// Name is the cluster name.
	Name string `yaml:"name"`
	// Node is this node's name in the node list.
	Node string `yaml:"node"`
	// NodesFile is the node list file, the same on every node.
	NodesFile string `yaml:"nodes_file"`
}

// Auth configures logins and tokens.
type Auth struct {
	// UsersDir holds one <user>.yaml file per user.
	UsersDir string `yaml:"users_dir"`
	// KeysFile holds the token signing keys.
	KeysFile string `yaml:"keys_file"`
	// TokenTTL is how long a token is valid.
	TokenTTL units.Duration `yaml:"token_ttl"`
	// LoginRate limits login attempts per client address.
	LoginRate Rate `yaml:"login_rate"`
}

// Log configures logging.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
}

// Rate is a number of events allowed per period, written <count>/<s|m|h>.
type Rate struct {
	Count int
	Per   time.Duration
}

// UnmarshalText parses a rate such as "10/m".
func (r *Rate) UnmarshalText(text []byte) error {
	parsed, err := parseRate(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

func parseRate(text string) (Rate, error) {
	count, unit, ok := strings.Cut(text, "/")
	n, err := strconv.ParseUint(count, 10, 31)
	per, knownUnit := rateUnits(unit)
	if !ok || err != nil || !knownUnit {
		return Rate{}, fmt.Errorf("invalid rate %q: use <count>/<s|m|h>, for example 10/m", text)
	}
	return Rate{Count: int(n), Per: per}, nil
}

func rateUnits(unit string) (time.Duration, bool) {
	switch unit {
	case "s":
		return time.Second, true
	case "m":
		return time.Minute, true
	case "h":
		return time.Hour, true
	}
	return 0, false
}

// String renders the rate as <count>/<s|m|h>. A period that is not one second,
// minute or hour is rendered as <count>/<duration>.
func (r Rate) String() string {
	switch r.Per {
	case time.Second:
		return fmt.Sprintf("%d/s", r.Count)
	case time.Minute:
		return fmt.Sprintf("%d/m", r.Count)
	case time.Hour:
		return fmt.Sprintf("%d/h", r.Count)
	}
	return fmt.Sprintf("%d/%s", r.Count, r.Per)
}

// ShareNames returns the names of the shares in sorted order.
func (c *Config) ShareNames() []string {
	names := make([]string, 0, len(c.Shares))
	for name := range c.Shares {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// rawConfig is the shape of the file. It differs from Config only in
// trusted_proxies, which is read as text so that single addresses are accepted.
type rawConfig struct {
	Config         `yaml:",inline"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// Load reads <dir>/config.yaml, applies defaults, resolves relative paths
// against dir and validates the result. It does not check that the files and
// directories the configuration names exist. When the configuration is invalid
// the error lists every problem found.
func Load(dir string) (*Config, error) {
	file := filepath.Join(dir, FileName)
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read server config: %w", err)
	}
	raw, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	cfg := &raw.Config
	var problems []error
	var proxyProblems []error
	cfg.TrustedProxies, proxyProblems = parseProxies(raw.TrustedProxies)
	problems = append(problems, proxyProblems...)
	applyDefaults(cfg)
	base, err := normalizeBasePath(cfg.BasePath)
	if err != nil {
		problems = append(problems, err)
	} else {
		cfg.BasePath = base
	}
	resolvePaths(cfg, dir)
	problems = append(problems, validate(cfg)...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid server config %s: %w", file, errors.Join(problems...))
	}
	return cfg, nil
}

// decode parses exactly one YAML document and rejects unknown keys.
func decode(data []byte) (*rawConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	var next yaml.Node
	err := dec.Decode(&next)
	switch {
	case err == nil:
		return nil, errors.New("more than one YAML document")
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	return &raw, nil
}

// parseProxies converts trusted_proxies entries to prefixes. A single address
// becomes a prefix that covers only that address.
func parseProxies(entries []string) ([]netip.Prefix, []error) {
	var prefixes []netip.Prefix
	var problems []error
	for i, entry := range entries {
		prefix, err := parseProxy(entry)
		if err != nil {
			problems = append(problems, fmt.Errorf("trusted_proxies[%d]: %w", i, err))
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, problems
}

func parseProxy(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		return netip.ParsePrefix(entry)
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// applyDefaults fills in the values of keys that are absent. A zero token_ttl,
// mtime_precision or login_rate counts as absent; zero is not valid for any of
// them.
func applyDefaults(cfg *Config) {
	if cfg.BasePath == "" {
		cfg.BasePath = defaultBasePath
	}
	for name, share := range cfg.Shares {
		if share.Exclude == nil {
			share.Exclude = []string{defaultExclude}
			cfg.Shares[name] = share
		}
	}
	if cfg.Auth.TokenTTL == 0 {
		cfg.Auth.TokenTTL = units.Duration(defaultTokenTTL)
	}
	if cfg.Auth.LoginRate == (Rate{}) {
		cfg.Auth.LoginRate = Rate{Count: 10, Per: time.Minute}
	}
	if cfg.MTimePrecision == 0 {
		cfg.MTimePrecision = units.Duration(defaultMTimePrecision)
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = defaultLogFormat
	}
}

// normalizeBasePath returns the base path without its trailing slash, so that
// "/" becomes "", the API at the root.
func normalizeBasePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return p, fmt.Errorf("base_path: %q must start with \"/\"", p)
	}
	return strings.TrimRight(p, "/"), nil
}

// resolvePaths makes every path in cfg relative to dir unless it is absolute,
// and cleans it. Empty paths stay empty.
func resolvePaths(cfg *Config, dir string) {
	for i := range cfg.Listen {
		if tls := cfg.Listen[i].TLS; tls != nil {
			tls.CertFile = resolve(dir, tls.CertFile)
			tls.KeyFile = resolve(dir, tls.KeyFile)
		}
	}
	for name, share := range cfg.Shares {
		share.Path = resolve(dir, share.Path)
		cfg.Shares[name] = share
	}
	cfg.Cluster.NodesFile = resolve(dir, cfg.Cluster.NodesFile)
	cfg.Auth.UsersDir = resolve(dir, cfg.Auth.UsersDir)
	cfg.Auth.KeysFile = resolve(dir, cfg.Auth.KeysFile)
}

func resolve(dir, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// validate returns every problem in cfg, each naming its key.
func validate(cfg *Config) []error {
	var problems []error
	problems = append(problems, validateListen(cfg.Listen)...)
	problems = append(problems, validateShares(cfg)...)
	problems = append(problems, required("cluster.name", cfg.Cluster.Name)...)
	problems = append(problems, required("cluster.node", cfg.Cluster.Node)...)
	problems = append(problems, required("cluster.nodes_file", cfg.Cluster.NodesFile)...)
	problems = append(problems, required("auth.users_dir", cfg.Auth.UsersDir)...)
	problems = append(problems, required("auth.keys_file", cfg.Auth.KeysFile)...)
	problems = append(problems, validateAuth(cfg.Auth)...)
	problems = append(problems, validateAtLeast("mtime_precision", cfg.MTimePrecision, minMTimePrecision)...)
	problems = append(problems, oneOf("log.level", cfg.Log.Level, "debug", "info", "warn", "error")...)
	problems = append(problems, oneOf("log.format", cfg.Log.Format, "text", "json")...)
	return problems
}

// required reports an empty value.
func required(key, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: must not be empty", key)}
	}
	return nil
}

// oneOf reports a value that is not one of the allowed ones.
func oneOf(key, value string, allowed ...string) []error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return []error{fmt.Errorf("%s: %q is not one of %s", key, value, strings.Join(allowed, ", "))}
}

// validateAtLeast reports a duration that is unlimited or shorter than min.
func validateAtLeast(key string, d units.Duration, floor time.Duration) []error {
	if d.Unlimited() || d.Std() < floor {
		return []error{fmt.Errorf("%s: %s is not allowed, use a finite duration of at least %s", key, d, units.Duration(floor))}
	}
	return nil
}

func validateListen(listeners []Listener) []error {
	if len(listeners) == 0 {
		return []error{errors.New("listen: at least one entry is required")}
	}
	var problems []error
	seen := make(map[netip.AddrPort]int)
	for i, l := range listeners {
		key := fmt.Sprintf("listen[%d]", i)
		validAddr := l.Address.IsValid()
		validPort := l.Port >= 1 && l.Port <= 65535
		if !validAddr {
			problems = append(problems, fmt.Errorf("%s.address: must not be empty", key))
		}
		if !validPort {
			problems = append(problems, fmt.Errorf("%s.port: %d is not between 1 and 65535", key, l.Port))
		}
		if validAddr && validPort {
			ap := netip.AddrPortFrom(l.Address, uint16(l.Port)) //nolint:gosec // the port range was checked above
			if first, dup := seen[ap]; dup {
				problems = append(problems, fmt.Errorf("%s: %s is already listed in listen[%d]", key, ap, first))
			} else {
				seen[ap] = i
			}
		}
		if l.TLS != nil && (l.TLS.CertFile == "") != (l.TLS.KeyFile == "") {
			problems = append(problems, fmt.Errorf("%s.tls: cert_file and key_file must both be set or both be left out", key))
		}
	}
	return problems
}

func validateShares(cfg *Config) []error {
	if len(cfg.Shares) == 0 {
		return []error{errors.New("shares: at least one share is required")}
	}
	var problems []error
	for _, name := range cfg.ShareNames() {
		problems = append(problems, validateShare(name, cfg.Shares[name])...)
	}
	return problems
}

func validateShare(name string, share Share) []error {
	key := "shares." + name
	var problems []error
	if !namePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf("%s: name must match %s", key, namePattern))
	}
	problems = append(problems, required(key+".path", share.Path)...)
	for i, pattern := range share.Exclude {
		if err := validateExclude(pattern); err != nil {
			problems = append(problems, fmt.Errorf("%s.exclude[%d]: %w", key, i, err))
		}
	}
	if share.MinAge < 0 || share.MinAge.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_age: %s is not allowed, use a finite duration of at least 0", key, share.MinAge))
	}
	return problems
}

// validateExclude checks that pattern is a valid glob for a single name.
func validateExclude(pattern string) error {
	if strings.Contains(pattern, "/") {
		return fmt.Errorf("%q must not contain \"/\"", pattern)
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("%q: %w", pattern, err)
	}
	return nil
}

func validateAuth(auth Auth) []error {
	problems := validateAtLeast("auth.token_ttl", auth.TokenTTL, minTokenTTL)
	if auth.LoginRate.Count < 1 {
		problems = append(problems, fmt.Errorf("auth.login_rate: count %d is not at least 1", auth.LoginRate.Count))
	}
	return problems
}
