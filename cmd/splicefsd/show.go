package main

import (
	"errors"
	"io"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/common/units"
	"github.com/SpliceFS/server/config"
)

// shownConfig is what config show prints: the settings, the proxies that the settings
// keep as prefixes, and the shares.
type shownConfig struct {
	config.Config  `yaml:",inline"`
	TrustedProxies []string              `yaml:"trusted_proxies"`
	Shares         map[string]shownShare `yaml:"shares"`
}

// shownShare holds only what differs from the defaults of a share, so that the output of a
// share says what its file changed.
type shownShare struct {
	Path           string         `yaml:"path"`
	Exclude        *[]string      `yaml:"exclude,omitempty"`
	MinAge         units.Duration `yaml:"min_age,omitempty"`
	FollowSymlinks bool           `yaml:"follow_symlinks,omitempty"`
}

// showConfig writes the merged configuration as YAML. A value that a file set ends with a
// comment naming that file; a value nobody set is a default and has none.
func showConfig(w io.Writer, cfg *config.Config) error {
	shown := shownConfig{Config: *cfg, Shares: make(map[string]shownShare, len(cfg.Shares))}
	for _, p := range cfg.TrustedProxies {
		shown.TrustedProxies = append(shown.TrustedProxies, p.String())
	}
	for name, s := range cfg.Shares {
		shown.Shares[name] = shownShare{
			Path:           s.Path,
			Exclude:        nonDefaultExclude(s.Exclude),
			MinAge:         s.MinAge,
			FollowSymlinks: s.FollowSymlinks,
		}
	}
	// Encoding these types cannot fail; if it ever did, the encoder below reports the empty
	// node and this error together.
	var doc yaml.Node
	encodeErr := doc.Encode(shown)
	annotate(&doc, "", cfg.Sources)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	return errors.Join(encodeErr, enc.Encode(&doc))
}

// annotate puts the file of every value in sources behind that value; n is a mapping. A list
// with items is annotated on its key, because a comment after its first item would be
// misread, and a list without items on the value, because a comment on its key moves to the
// next line.
func annotate(n *yaml.Node, prefix string, sources map[string]string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + path
		}
		file, set := sources[path]
		switch {
		case value.Kind == yaml.MappingNode:
			annotate(value, path, sources)
		case set && len(value.Content) == 0:
			value.LineComment = "# " + file
		case set:
			key.LineComment = "# " + file
		}
	}
}

// nonDefaultExclude returns nil, which is left out of the output, for the default patterns.
func nonDefaultExclude(exclude []string) *[]string {
	if slices.Equal(exclude, config.DefaultExclude()) {
		return nil
	}
	return &exclude
}
