package cluster

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// urlSeeds are the starting inputs of FuzzCleanURL: valid forms, and each way a URL can be
// wrong.
func urlSeeds() []string {
	return []string{
		"https://east.example.com/api", "http://west.example.com:8480/api/", "https://h/",
		"https://h////", "", "ftp://h", "//h", "/path", "https://", "https:///x", "https://h?q=1",
		"https://h#f", "https://h/?", "https://h/#", "http://[::1]:80/", "http://[::1",
		"https://h:99999",
		"HTTP://H", "https://u:p@h/", "https://h/%zz", "https://h/a b", "\x00", "https://h\x00",
	}
}

// checkURLForm fails the test when got, the cleaned form of raw, is not what cleanURL promises.
func checkURLForm(t *testing.T, raw, got string) {
	t.Helper()
	// Node URLs are used to build requests to other nodes, so each must be an absolute
	// http or https URL with a host and no query or fragment.
	parsed, parseErr := url.Parse(got)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		t.Fatalf("cleanURL(%q) = %q, which is not an absolute http URL (%v)", raw, got, parseErr)
	}
	if strings.ContainsAny(got, "?#") || strings.HasSuffix(got, "/") {
		t.Fatalf("cleanURL(%q) = %q", raw, got)
	}
}

// checkCleanURL fails the test when cleanURL breaks its promises for raw.
func checkCleanURL(t *testing.T, raw string) {
	t.Helper()
	got, err := cleanURL(raw)
	if err != nil {
		if got != "" {
			t.Fatalf("cleanURL(%q) failed but returned %q", raw, got)
		}
		return
	}
	checkURLForm(t, raw, got)
	// Cleaning twice must change nothing, or a reload would alter the list.
	if again, err := cleanURL(got); err != nil || again != got {
		t.Fatalf("cleanURL(%q) = %q, then %q, %v", raw, got, again, err)
	}
}

func FuzzCleanURL(f *testing.F) {
	for _, seed := range urlSeeds() {
		f.Add(seed)
	}
	f.Fuzz(checkCleanURL)
}

// nodeFileSeeds are the starting inputs of FuzzNodeFile: a valid file, and each way a node
// file can be wrong.
func nodeFileSeeds() []string {
	return []string{
		nodeContent("east", "https://east.example.com/api"), "", "---\n", "{}", "name: a\n",
		"url: https://a\n", "[", "a: &a [*a]\n", "\x00", "name: [\n",
		nodeContent("A", "https://a"), nodeContent("a", "ftp://a"),
		nodeContent("a", "https://a?q"), nodeContent("a", "https://a#f"),
		nodeContent("a", "https:///a"), nodeContent("''", "https://a"),
		nodeContent(strings.Repeat("a", 65), "https://a"), nodeContent("a", "https://a///"),
		nodeContent("a", "https://a") + "extra: 1\n",
		nodeContent("a", "https://a") + "---\n" + nodeContent("b", "https://b"),
	}
}

// checkNodeFile writes content to path and fails the test when a loader gives anything but
// at most one valid node for it.
func checkNodeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// A new loader each time, so that no earlier content is cached.
	nodes, err := NewLoader(filepath.Dir(path), slog.New(slog.DiscardHandler)).Nodes()
	if err != nil {
		t.Fatalf("Nodes failed on %q: %v", content, err)
	}
	// One file gives at most one node, and a bad file gives none, so no caller can use
	// half of a bad file.
	if len(nodes.Nodes) > 1 {
		t.Fatalf("one file gave %d nodes: %q", len(nodes.Nodes), content)
	}
	// A node that loads has a valid name and a clean URL, because clients pick nodes by
	// name and send requests to the URL.
	for _, node := range nodes.Nodes {
		if !namePattern.MatchString(node.Name) {
			t.Fatalf("accepted node name %q: %q", node.Name, content)
		}
		if again, err := cleanURL(node.URL); err != nil || again != node.URL {
			t.Fatalf("accepted node URL %q: %v", node.URL, err)
		}
	}
}

func FuzzNodeFile(f *testing.F) {
	path := filepath.Join(f.TempDir(), nodeFile(1))
	for _, seed := range nodeFileSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, content []byte) { checkNodeFile(t, path, content) })
}
