// Package cluster serves the list of nodes that make up a SpliceFS cluster. Every node
// announces itself by writing one file into the shared nodes directory, and the list is the
// union of the files, so no file has more than one writer.
package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/internal/datadir"
)

// namePattern is the form of a node name.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// The errors of a bad node file. They only reach the log, where the problem file is named,
// so they are not exported. Each message that follows a quoted value holds only the words
// after that value, which keeps the text of the log lines as it was.
var (
	errNotNodeFile = errors.New("the file name is not a node id and .yaml")
	errNameForm    = errors.New("must match")
	errURLNotHTTP  = errors.New("must be an absolute http or https URL")
	errURLNoHost   = errors.New("has no host")
	errURLExtra    = errors.New("must not have a query or a fragment")
)

// announcementMode lets every user on the machine and every sync tool read the file: it holds
// only what clients are told anyway.
const announcementMode = 0o644

// Announce writes the file of the node nodeID into the nodes directory dir: its name and its
// public URL. The file is replaced only when its content differs, so that a restart does not
// make a sync tool copy it again. A node with no URL is not announced, so its file is
// removed. The directory must exist.
func Announce(dir, nodeID string, self wire.Node) error {
	path := filepath.Join(dir, datadir.NodeFile(nodeID))
	if self.URL == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove node file: %w", err)
		}
		return nil
	}
	// A struct of two strings always encodes, so there is no error to handle.
	data, _ := yaml.Marshal(self) //nolint:errcheck // cannot fail for a struct of two strings
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return nil
	}
	if err := datadir.WriteAtomic(path, data, announcementMode); err != nil {
		return fmt.Errorf("write node file: %w", err)
	}
	return nil
}

// Loader serves the union of the node files of a directory. It is safe for concurrent use.
type Loader struct {
	log *slog.Logger

	mu     sync.Mutex
	files  *datadir.Dir[wire.Node]
	nodes  []wire.Node
	merged bool
}

// NewLoader returns a Loader for the nodes directory dir. Problems with a file are logged
// to log.
func NewLoader(dir string, log *slog.Logger) *Loader {
	return &Loader{log: log, files: datadir.NewDir(dir, parseNode, log)}
}

// Nodes returns the nodes of every valid file, sorted by node name; the list may be empty.
// The cluster name is not known here, so the field is empty. A file that is invalid is
// ignored with a warning; of two files with the same node name, the file that sorts first
// by name is used. The result does not share memory with the cache.
func (l *Loader) Nodes() (wire.Nodes, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, changed := l.files.Refresh()
	if changed || !l.merged {
		l.nodes, l.merged = l.merge(entries), true
	}
	return wire.Nodes{Nodes: slices.Clone(l.nodes)}, nil
}

// merge builds the list from the files in name order.
func (l *Loader) merge(entries []datadir.Entry[wire.Node]) []wire.Node {
	nodes := make([]wire.Node, 0, len(entries))
	owner := map[string]string{}
	for _, entry := range entries {
		if first, dup := owner[entry.Value.Name]; dup {
			l.log.Warn("duplicate node name, keeping the first file's node",
				slog.String("name", entry.Value.Name), slog.String("kept", first),
				slog.String("ignored", entry.Name))
			continue
		}
		owner[entry.Value.Name] = entry.Name
		nodes = append(nodes, entry.Value)
	}
	slices.SortFunc(nodes, func(a, b wire.Node) int { return strings.Compare(a.Name, b.Name) })
	return nodes
}

// parseNode validates one node file. The name of the file must be a node id, so a stray
// file never adds a node.
func parseNode(name string, data []byte) (wire.Node, error) {
	if _, ok := datadir.NodeOfFile(name); !ok {
		return wire.Node{}, errNotNodeFile
	}
	var node wire.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&node); err != nil {
		return wire.Node{}, fmt.Errorf("parse node file: %w", err)
	}
	if !namePattern.MatchString(node.Name) {
		return wire.Node{}, fmt.Errorf("name %q %w %s", node.Name, errNameForm, namePattern)
	}
	cleaned, err := cleanURL(node.URL)
	if err != nil {
		return wire.Node{}, fmt.Errorf("url: %w", err)
	}
	node.URL = cleaned
	return node, nil
}

// cleanURL checks that raw is an absolute http or https URL with a host and no
// query or fragment, and returns it without a trailing slash.
func cleanURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is not a valid URL: %w", raw, err)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return "", fmt.Errorf("%q %w", raw, errURLNotHTTP)
	case parsed.Hostname() == "":
		return "", fmt.Errorf("%q %w", raw, errURLNoHost)
	case strings.ContainsAny(raw, "?#"):
		return "", fmt.Errorf("%q %w", raw, errURLExtra)
	}
	return strings.TrimRight(raw, "/"), nil
}
