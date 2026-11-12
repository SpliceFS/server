package cluster

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzCleanURL(f *testing.F) {
	for _, s := range []string{
		"https://east.example.com/api", "http://west.example.com:8480/api/", "https://h/",
		"https://h////", "", "ftp://h", "//h", "/path", "https://", "https:///x", "https://h?q=1",
		"https://h#f", "https://h/?", "https://h/#", "http://[::1]:80/", "http://[::1",
		"https://h:99999",
		"HTTP://H", "https://u:p@h/", "https://h/%zz", "https://h/a b", "\x00", "https://h\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := cleanURL(raw)
		if err != nil {
			if got != "" {
				t.Fatalf("cleanURL(%q) failed but returned %q", raw, got)
			}
			return
		}
		// Node URLs are used to build requests to other nodes, so each must be an absolute
		// http or https URL with a host and no query or fragment.
		u, perr := url.Parse(got)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			t.Fatalf("cleanURL(%q) = %q, which is not an absolute http URL (%v)", raw, got, perr)
		}
		if strings.ContainsAny(got, "?#") || strings.HasSuffix(got, "/") {
			t.Fatalf("cleanURL(%q) = %q", raw, got)
		}
		// Cleaning twice must change nothing, or a reload would alter the list.
		if again, err := cleanURL(got); err != nil || again != got {
			t.Fatalf("cleanURL(%q) = %q, then %q, %v", raw, got, again, err)
		}
	})
}

func FuzzNodeFile(f *testing.F) {
	dir := f.TempDir()
	path := filepath.Join(dir, nodeFile(1))
	for _, s := range []string{
		nodeContent("east", "https://east.example.com/api"), "", "---\n", "{}", "name: a\n",
		"url: https://a\n", "[", "a: &a [*a]\n", "\x00", "name: [\n",
		nodeContent("A", "https://a"), nodeContent("a", "ftp://a"),
		nodeContent("a", "https://a?q"), nodeContent("a", "https://a#f"),
		nodeContent("a", "https:///a"), nodeContent("''", "https://a"),
		nodeContent(strings.Repeat("a", 65), "https://a"), nodeContent("a", "https://a///"),
		nodeContent("a", "https://a") + "extra: 1\n",
		nodeContent("a", "https://a") + "---\n" + nodeContent("b", "https://b"),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		// A new loader each time, so that no earlier content is cached.
		nodes, err := NewLoader(dir, slog.New(slog.DiscardHandler)).Nodes()
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
		for _, n := range nodes.Nodes {
			if !namePattern.MatchString(n.Name) {
				t.Fatalf("accepted node name %q: %q", n.Name, content)
			}
			if again, err := cleanURL(n.URL); err != nil || again != n.URL {
				t.Fatalf("accepted node URL %q: %v", n.URL, err)
			}
		}
	})
}
