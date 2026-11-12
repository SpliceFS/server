package config

import (
	"net/netip"

	"github.com/SpliceFS/common/units"
)

// Defaults of the directory-based settings.
const (
	defaultPort        = 8480
	defaultClusterName = "default"
)

// Settings is the server configuration read from a configuration directory: the merged
// conf.d files and the shares of shares.d.
type Settings struct {
	// Listen lists the addresses and ports to serve on.
	Listen []Endpoint `yaml:"listen"`
	// BasePath is the URL prefix of the API: "" for the root, otherwise a path
	// that starts with a slash and does not end with one.
	BasePath string `yaml:"base_path"`
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is believed.
	TrustedProxies []netip.Prefix `yaml:"-"`
	// Cluster identifies this node and its cluster.
	Cluster ClusterSettings `yaml:"cluster"`
	// Auth configures logins and tokens.
	Auth AuthSettings `yaml:"auth"`
	// MTimePrecision is the precision at which modification times are compared.
	MTimePrecision units.Duration `yaml:"mtime_precision"`
	// Log configures logging.
	Log Log `yaml:"log"`
	// Shares maps a share name to its settings. It is never nil.
	Shares map[string]Share `yaml:"-"`
	// Sources maps the dotted key path of every value that a file set (for example
	// "auth.token_ttl", "listen", "shares.media.path") to the base name of that file.
	// Defaults have no entry.
	Sources map[string]string `yaml:"-"`
}

// Endpoint is one listener with its choice between TLS and plain HTTP.
type Endpoint struct {
	Listener `yaml:",inline"`
	// Plain serves without TLS, for use behind a reverse proxy that terminates it.
	Plain bool `yaml:"plain"`
}

// ClusterSettings names the cluster and announces this node.
type ClusterSettings struct {
	// Name is the cluster name.
	Name string `yaml:"name"`
	// Node is the display name of this node.
	Node string `yaml:"node"`
	// URL is the public URL of this node as clients reach it, empty when not announced.
	URL string `yaml:"url"`
}

// AuthSettings configures logins and tokens.
type AuthSettings struct {
	// TokenTTL is how long a token is valid.
	TokenTTL units.Duration `yaml:"token_ttl"`
	// LoginRate limits login attempts per client address.
	LoginRate Rate `yaml:"login_rate"`
}

// rawSettings reads trusted_proxies as text so that single addresses are accepted.
type rawSettings struct {
	Settings       `yaml:",inline"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}
