// Package config loads and validates the configuration of the SpliceFS server from a
// configuration directory: settings files in conf.d and one file per share in shares.d.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/common/units"
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

// Listener is one address and port to serve on.
type Listener struct {
	Address netip.Addr `yaml:"address"`
	Port    int        `yaml:"port"`
	// TLS is nil when a reverse proxy terminates TLS.
	TLS *TLS `yaml:"tls,omitempty"`
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

// MarshalText renders the rate as String does, so that it is written the way it is read.
func (r Rate) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

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

// rejectSecondDocument fails when dec has another YAML document to read.
func rejectSecondDocument(dec *yaml.Decoder) error {
	var next yaml.Node
	err := dec.Decode(&next)
	switch {
	case err == nil:
		return errors.New("more than one YAML document")
	case !errors.Is(err, io.EOF):
		return err
	}
	return nil
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

// DefaultExclude returns the exclude patterns of a share that sets none: names that start
// with a dot are hidden.
func DefaultExclude() []string { return []string{defaultExclude} }

// applyShareDefaults fills in the share keys that are absent.
func applyShareDefaults(share *Share) {
	if share.Exclude == nil {
		share.Exclude = DefaultExclude()
	}
}

// applyLimitDefaults fills in the token lifetime, login rate and mtime precision.
func applyLimitDefaults(ttl *units.Duration, rate *Rate, mtime *units.Duration) {
	if *ttl == 0 {
		*ttl = units.Duration(defaultTokenTTL)
	}
	if *rate == (Rate{}) {
		*rate = Rate{Count: 10, Per: time.Minute}
	}
	if *mtime == 0 {
		*mtime = units.Duration(defaultMTimePrecision)
	}
}

// applyLogDefaults fills in the log keys that are absent.
func applyLogDefaults(l *Log) {
	if l.Level == "" {
		l.Level = defaultLogLevel
	}
	if l.Format == "" {
		l.Format = defaultLogFormat
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

// resolveTLS makes the certificate and key paths of l relative to dir.
func resolveTLS(l *Listener, dir string) {
	if l.TLS != nil {
		l.TLS.CertFile = resolve(dir, l.TLS.CertFile)
		l.TLS.KeyFile = resolve(dir, l.TLS.KeyFile)
	}
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

// validateLog checks the log keys.
func validateLog(l Log) []error {
	problems := oneOf("log.level", l.Level, "debug", "info", "warn", "error")
	return append(problems, oneOf("log.format", l.Format, "text", "json")...)
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

// validateLimits checks the token lifetime, login rate and mtime precision.
func validateLimits(ttl units.Duration, rate Rate, mtime units.Duration) []error {
	problems := validateAtLeast("auth.token_ttl", ttl, minTokenTTL)
	if rate.Count < 1 {
		problems = append(problems, fmt.Errorf("auth.login_rate: count %d is not at least 1", rate.Count))
	}
	return append(problems, validateAtLeast("mtime_precision", mtime, minMTimePrecision)...)
}
