// Package cluster serves the node list file, the list of nodes that make up a
// SpliceFS cluster (see "Node list" in docs/configuration.md).
package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/BlueRival/splicefs/internal/wire"
)

// namePattern is the form of a node name.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Loader reads a node list file and caches the result until the file changes.
// It is safe for concurrent use.
type Loader struct {
	path string

	mu     sync.Mutex
	cached *snapshot
}

// snapshot is a successfully loaded node list and the state of the file it was
// read from.
type snapshot struct {
	modTime time.Time
	size    int64
	nodes   wire.Nodes
}

// NewLoader returns a Loader for the node list at path.
func NewLoader(path string) *Loader {
	return &Loader{path: path}
}

// Nodes returns the node list. The file is checked on every call and is read
// and validated again only when its modification time or size has changed since
// the last successful load. A failed load returns an error and leaves nothing
// cached, so the next call tries again. The result does not share memory with
// the cache.
func (l *Loader) Nodes() (wire.Nodes, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	info, err := os.Stat(l.path)
	if err != nil {
		l.cached = nil
		return wire.Nodes{}, fmt.Errorf("stat node list: %w", err)
	}
	if c := l.cached; c != nil && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return clone(c.nodes), nil
	}
	l.cached = nil
	nodes, err := read(l.path)
	if err != nil {
		return wire.Nodes{}, err
	}
	l.cached = &snapshot{modTime: info.ModTime(), size: info.Size(), nodes: nodes}
	return clone(nodes), nil
}

// clone returns a copy of nodes that shares no memory with it.
func clone(nodes wire.Nodes) wire.Nodes {
	nodes.Nodes = slices.Clone(nodes.Nodes)
	return nodes
}

// read loads and validates the node list file at path.
func read(path string) (wire.Nodes, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return wire.Nodes{}, fmt.Errorf("read node list: %w", err)
	}
	var nodes wire.Nodes
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&nodes); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("file is empty")
		}
		return wire.Nodes{}, fmt.Errorf("parse node list %s: %w", path, err)
	}
	if err := normalize(&nodes); err != nil {
		return wire.Nodes{}, fmt.Errorf("invalid node list %s: %w", path, err)
	}
	return nodes, nil
}

// normalize validates nodes, reporting every problem, and removes the trailing
// slash from each URL.
func normalize(nodes *wire.Nodes) error {
	var problems []error
	if nodes.Cluster == "" {
		problems = append(problems, errors.New("cluster: must not be empty"))
	}
	if len(nodes.Nodes) == 0 {
		problems = append(problems, errors.New("nodes: at least one node is required"))
	}
	seen := make(map[string]bool)
	for i := range nodes.Nodes {
		node := &nodes.Nodes[i]
		key := fmt.Sprintf("nodes[%d]", i)
		switch {
		case !namePattern.MatchString(node.Name):
			problems = append(problems, fmt.Errorf("%s.name: %q must match %s", key, node.Name, namePattern))
		case seen[node.Name]:
			problems = append(problems, fmt.Errorf("%s.name: %q is used more than once", key, node.Name))
		}
		seen[node.Name] = true
		cleaned, err := cleanURL(node.URL)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s.url: %w", key, err))
			continue
		}
		node.URL = cleaned
	}
	return errors.Join(problems...)
}

// cleanURL checks that raw is an absolute http or https URL with a host and no
// query or fragment, and returns it without a trailing slash.
func cleanURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is not a valid URL: %w", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q must be an absolute http or https URL", raw)
	case u.Hostname() == "":
		return "", fmt.Errorf("%q has no host", raw)
	case strings.ContainsAny(raw, "?#"):
		return "", fmt.Errorf("%q must not have a query or a fragment", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
