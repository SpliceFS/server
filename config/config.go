// Package config loads and validates the configuration of the SpliceFS server from a
// configuration directory: settings files in conf.d and one file per share in shares.d.
package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
	// defaultLoginRateCount is the number of login attempts per defaultLoginRatePeriod.
	defaultLoginRateCount  = 10
	defaultLoginRatePeriod = time.Minute
)

// The range of a TCP port. Port 0 asks the system to choose one, which a server that
// announces its address cannot allow.
const (
	minPort = 1
	maxPort = math.MaxUint16
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
	count, suffix, ok := strings.Cut(text, "/")
	n, err := strconv.ParseUint(count, 10, 31)
	per, knownUnit := periodOf(suffix)
	if !ok || err != nil || !knownUnit {
		return Rate{}, fmt.Errorf("invalid rate %q: %w", text, ErrInvalidRate)
	}
	return Rate{Count: int(n), Per: per}, nil
}

// rateUnit pairs the suffix of a rate with the period it stands for.
type rateUnit struct {
	suffix string
	period time.Duration
}

// rateUnits lists the periods a rate may use, once for reading and writing. It is a
// function so that no package variable holds a list that code could change.
func rateUnits() []rateUnit {
	return []rateUnit{{"s", time.Second}, {"m", time.Minute}, {"h", time.Hour}}
}

// periodOf returns the period a rate suffix stands for.
func periodOf(suffix string) (time.Duration, bool) {
	for _, unit := range rateUnits() {
		if unit.suffix == suffix {
			return unit.period, true
		}
	}
	return 0, false
}

// suffixOf returns the rate suffix for a period.
func suffixOf(period time.Duration) (string, bool) {
	for _, unit := range rateUnits() {
		if unit.period == period {
			return unit.suffix, true
		}
	}
	return "", false
}

// MarshalText renders the rate as String does, so that it is written the way it is read.
func (r Rate) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// String renders the rate as <count>/<s|m|h>. A period that is not one second,
// minute or hour is rendered as <count>/<duration>.
func (r Rate) String() string {
	period := r.Per.String()
	if suffix, known := suffixOf(r.Per); known {
		period = suffix
	}
	return fmt.Sprintf("%d/%s", r.Count, period)
}

// rejectSecondDocument fails when dec has another YAML document to read.
//
//nolint:wrapcheck // decodeFile names the file; another prefix would change the message the administrator reads
func rejectSecondDocument(dec *yaml.Decoder) error {
	var next yaml.Node
	err := dec.Decode(&next)
	switch {
	case err == nil:
		return ErrMultipleDocuments
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

// parseProxy returns the error of net/netip as it is: it names the function and the entry,
// and parseProxies adds the position in the list.
//
//nolint:wrapcheck // another prefix would change the message the administrator reads
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
		*rate = Rate{Count: defaultLoginRateCount, Per: defaultLoginRatePeriod}
	}
	if *mtime == 0 {
		*mtime = units.Duration(defaultMTimePrecision)
	}
}

// applyLogDefaults fills in the log keys that are absent.
func applyLogDefaults(logConfig *Log) {
	if logConfig.Level == "" {
		logConfig.Level = defaultLogLevel
	}
	if logConfig.Format == "" {
		logConfig.Format = defaultLogFormat
	}
}

// normalizeBasePath returns the base path without its trailing slash, so that
// "/" becomes "", the API at the root.
func normalizeBasePath(basePath string) (string, error) {
	if !strings.HasPrefix(basePath, "/") {
		return basePath, fmt.Errorf("base_path: %q %w", basePath, ErrBasePathNoSlash)
	}
	return strings.TrimRight(basePath, "/"), nil
}

// resolveTLS makes the certificate and key paths of listener relative to dir.
func resolveTLS(listener *Listener, dir string) {
	if listener.TLS != nil {
		listener.TLS.CertFile = resolve(dir, listener.TLS.CertFile)
		listener.TLS.KeyFile = resolve(dir, listener.TLS.KeyFile)
	}
}

func resolve(dir, target string) string {
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target)
}

// validateLog checks the log keys.
func validateLog(logConfig Log) []error {
	problems := oneOf("log.level", logConfig.Level, "debug", "info", "warn", "error")
	return append(problems, oneOf("log.format", logConfig.Format, "text", "json")...)
}

// required reports an empty value.
func required(key, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: %w", key, ErrEmpty)}
	}
	return nil
}

// oneOf reports a value that is not one of the allowed ones.
func oneOf(key, value string, allowed ...string) []error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return []error{fmt.Errorf("%s: %q %w %s", key, value, ErrNotOneOf, strings.Join(allowed, ", "))}
}

// validateAtLeast reports a duration that is unlimited or shorter than min.
func validateAtLeast(key string, d units.Duration, floor time.Duration) []error {
	if d.Unlimited() || d.Std() < floor {
		return []error{fmt.Errorf("%s: %s %w %s", key, d, ErrDurationTooShort, units.Duration(floor))}
	}
	return nil
}

// validateListen checks every listener and that no two share an address and port.
func validateListen(listeners []Listener) []error {
	if len(listeners) == 0 {
		return []error{ErrNoListener}
	}
	var problems []error
	seen := make(map[netip.AddrPort]int)
	for i, listener := range listeners {
		problems = append(problems, validateListener(i, listener, seen)...)
	}
	return problems
}

// validateListener checks the listener at index of the listen list. seen maps the address
// and port of every earlier valid listener to its index.
func validateListener(index int, listener Listener, seen map[netip.AddrPort]int) []error {
	key := fmt.Sprintf("listen[%d]", index)
	problems := validateBinding(key, index, listener, seen)
	if listener.TLS != nil && (listener.TLS.CertFile == "") != (listener.TLS.KeyFile == "") {
		problems = append(problems, fmt.Errorf("%s.tls: %w", key, ErrTLSPairIncomplete))
	}
	return problems
}

// validateBinding checks the address and port of a listener and records them in seen.
func validateBinding(key string, index int, listener Listener, seen map[netip.AddrPort]int) []error {
	var problems []error
	validAddr := listener.Address.IsValid()
	validPort := listener.Port >= minPort && listener.Port <= maxPort
	if !validAddr {
		problems = append(problems, fmt.Errorf("%s.address: %w", key, ErrEmpty))
	}
	if !validPort {
		problems = append(problems, fmt.Errorf("%s.port: %d %w", key, listener.Port, ErrPortOutOfRange))
	}
	if !validAddr || !validPort {
		return problems
	}
	port := uint16(listener.Port) //nolint:gosec // the port range was checked above
	binding := netip.AddrPortFrom(listener.Address, port)
	if first, dup := seen[binding]; dup {
		return append(problems, fmt.Errorf("%s: %s %w listen[%d]", key, binding, ErrDuplicateListener, first))
	}
	seen[binding] = index
	return problems
}

func validateShare(name string, share Share) []error {
	key := "shares." + name
	var problems []error
	if !namePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf("%s: name %w %s", key, ErrNamePattern, namePattern))
	}
	problems = append(problems, required(key+".path", share.Path)...)
	for i, pattern := range share.Exclude {
		if err := validateExclude(pattern); err != nil {
			problems = append(problems, fmt.Errorf("%s.exclude[%d]: %w", key, i, err))
		}
	}
	if share.MinAge < 0 || share.MinAge.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_age: %s %w 0", key, share.MinAge, ErrDurationTooShort))
	}
	return problems
}

// validateExclude checks that pattern is a valid glob for a single name.
func validateExclude(pattern string) error {
	if strings.Contains(pattern, "/") {
		return fmt.Errorf("%q %w", pattern, ErrExcludeSlash)
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
		problems = append(problems, fmt.Errorf("auth.login_rate: count %d %w", rate.Count, ErrCountBelowOne))
	}
	return append(problems, validateAtLeast("mtime_precision", mtime, minMTimePrecision)...)
}
