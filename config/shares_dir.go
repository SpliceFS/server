package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// readShares reads one share per file of dir; the file name without .yaml is the share
// name. It records the file of every value that was set.
func readShares(dir string, sources map[string]string) (map[string]Share, []error) {
	shares := make(map[string]Share)
	names, err := yamlFiles(dir)
	if err != nil {
		return shares, []error{fmt.Errorf("%s: %w", dir, err)}
	}
	var problems []error
	for _, file := range names {
		var share Share
		keys, err := decodeFile(filepath.Join(dir, file), &share)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		name := strings.TrimSuffix(file, yamlExt)
		for _, key := range keys {
			sources["shares."+name+"."+key] = file
		}
		applyShareDefaults(&share)
		shares[name] = share
	}
	return shares, problems
}

// validateShareFiles checks every share, in name order, and names the file of each
// problem because the file is the share.
func validateShareFiles(shares map[string]Share) []error {
	names := make([]string, 0, len(shares))
	for name := range shares {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []error
	for _, name := range names {
		share := shares[name]
		found := validateShare(name, share)
		if share.Path != "" && !filepath.IsAbs(share.Path) {
			found = append(found, fmt.Errorf("shares.%s.path: %q must be absolute", name, share.Path))
		}
		for _, p := range found {
			problems = append(problems, fmt.Errorf("%w (file %s%s)", p, name, yamlExt))
		}
	}
	return problems
}
