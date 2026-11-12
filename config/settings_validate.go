package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// validateSettings returns every problem in s, each naming its key.
func validateSettings(s *Settings) []error {
	problems := validateEndpoints(s.Listen)
	problems = append(problems, validateNode(s.Cluster.Node)...)
	problems = append(problems, validateLimits(s.Auth.TokenTTL, s.Auth.LoginRate, s.MTimePrecision)...)
	return append(problems, validateLog(s.Log)...)
}

// validateEndpoints applies the listener rules both loaders share, then the rule that
// plain HTTP and TLS exclude each other.
func validateEndpoints(endpoints []Endpoint) []error {
	listeners := make([]Listener, len(endpoints))
	var problems []error
	for i, e := range endpoints {
		listeners[i] = e.Listener
		if e.Plain && e.TLS != nil {
			problems = append(problems,
				fmt.Errorf("listen[%d].plain: must not be true together with tls", i))
		}
	}
	return append(validateListen(listeners), problems...)
}

// validateNode applies the node name rule of the cluster package, which is the same
// pattern as the share name rule; it is not imported so that config stays a leaf.
func validateNode(node string) []error {
	if node == "" {
		return required("cluster.node", node)
	}
	if !namePattern.MatchString(node) {
		return []error{fmt.Errorf("cluster.node: %q must match %s", node, namePattern)}
	}
	return nil
}

// normalizeURL checks the public URL of this node and trims its trailing slash. An empty
// URL means the node is not announced.
func normalizeURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	ok := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		!strings.ContainsAny(raw, "?#")
	if !ok {
		return raw, fmt.Errorf(
			"cluster.url: %q must be an absolute http or https URL with a host, "+
				"without query or fragment", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// indexPattern finds the numeric index of a list key, which sources do not carry.
var indexPattern = regexp.MustCompile(`\[\d+\]`)

// attribute adds the name of the file that set the value an error is about. The error text
// starts with the key; the nearest key that a file set, looking at the parents, is used.
func attribute(err error, sources map[string]string) error {
	text, _, _ := strings.Cut(err.Error(), ":")
	key := indexPattern.ReplaceAllString(text, "")
	for key != "" {
		if file, ok := sources[key]; ok {
			return fmt.Errorf("%w (file %s)", err, file)
		}
		cut := strings.LastIndex(key, ".")
		if cut < 0 {
			break
		}
		key = key[:cut]
	}
	return err
}
