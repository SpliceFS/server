package cluster

import (
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
		"https://h#f", "https://h/?", "https://h/#", "http://[::1]:80/", "http://[::1", "https://h:99999",
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

func FuzzNodesFile(f *testing.F) {
	path := filepath.Join(f.TempDir(), "nodes.yaml")
	for _, s := range []string{
		validList, "", "---\n", "{}", "cluster: x\n", "cluster: x\nnodes: []\n", "[", "a: &a [*a]\n",
		"cluster: x\nnodes:\n  - name: a\n    url: https://a\n  - name: a\n    url: https://b\n",
		"cluster: x\nnodes:\n  - name: A\n    url: https://a\n",
		"cluster: x\nnodes:\n  - name: a\n    url: ftp://a\n",
		"cluster: x\nnodes:\n  - name: a\n    url: https://a?q\n",
		"cluster: x\nnodes:\n  - name: a\n    url: https://a\n    extra: 1\n",
		"cluster: x\nnodes:\n  - name: a\n    url: https://a\n---\ncluster: y\n",
		"cluster: ''\nnodes:\n  - name: a\n    url: https://a\n", "\x00",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		nodes, err := read(path)
		if err != nil {
			// A refused list yields nothing, so no caller can use half of a bad file.
			if nodes.Cluster != "" || nodes.Nodes != nil {
				t.Fatalf("read failed with %v but returned %+v", err, nodes)
			}
			return
		}
		// Every accepted list names its cluster, has at least one node and has unique node
		// names, because clients pick nodes by name and URL.
		if nodes.Cluster == "" || len(nodes.Nodes) == 0 {
			t.Fatalf("list accepted without a cluster or nodes: %q", content)
		}
		seen := map[string]bool{}
		for _, n := range nodes.Nodes {
			if !namePattern.MatchString(n.Name) || seen[n.Name] {
				t.Fatalf("list accepted node name %q: %q", n.Name, content)
			}
			seen[n.Name] = true
			if again, err := cleanURL(n.URL); err != nil || again != n.URL {
				t.Fatalf("list accepted node URL %q: %v", n.URL, err)
			}
		}
	})
}
