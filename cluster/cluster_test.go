package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/server/internal/wire"
)

const validList = `cluster: example
nodes:
  - name: east
    url: https://east.example.com/api
  - name: west
    url: http://west.example.com:8480/api/
`

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// writeList writes content to a new node list file with a fixed modification
// time offset from epoch and returns its path.
func writeList(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	rewrite(t, path, content, 0)
	return path
}

// rewrite replaces the content of path and sets its modification time to epoch
// plus offset.
func rewrite(t *testing.T, path, content string, offset time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch, epoch.Add(offset)); err != nil {
		t.Fatal(err)
	}
}

func TestNodes(t *testing.T) {
	t.Parallel()
	got, err := NewLoader(writeList(t, validList)).Nodes()
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	want := wire.Nodes{Cluster: "example", Nodes: []wire.Node{
		{Name: "east", URL: "https://east.example.com/api"},
		{Name: "west", URL: "http://west.example.com:8480/api"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Nodes = %+v, want %+v", got, want)
	}
}

func TestNodesURLForms(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"https://east.example.com", "https://east.example.com"},
		{"https://east.example.com/", "https://east.example.com"},
		{"HTTPS://east.example.com/api/", "HTTPS://east.example.com/api"},
		{"http://192.0.2.7:8480/a/b//", "http://192.0.2.7:8480/a/b"},
		{"http://[2001:db8::1]:8480/api", "http://[2001:db8::1]:8480/api"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			list := "cluster: c\nnodes:\n  - name: n\n    url: " + tc.in + "\n"
			got, err := NewLoader(writeList(t, list)).Nodes()
			if err != nil {
				t.Fatalf("Nodes: %v", err)
			}
			if got.Nodes[0].URL != tc.want {
				t.Errorf("url = %q, want %q", got.Nodes[0].URL, tc.want)
			}
		})
	}
}

func TestNodesInvalid(t *testing.T) {
	t.Parallel()
	node := func(name, url string) string { return "  - name: " + name + "\n    url: " + url + "\n" }
	tests := []struct {
		name string
		list string
		want []string
	}{
		{"empty file", "", []string{"file is empty"}},
		{"syntax error", "cluster: [", []string{"parse node list"}},
		{"unknown key", validList + "extra: 1\n", []string{"extra"}},
		{"unknown node key", "cluster: c\nnodes:\n  - name: a\n    url: http://a.example.com\n    weight: 3\n", []string{"weight"}},
		{"no cluster", "nodes:\n" + node("a", "http://a.example.com"), []string{"cluster: must not be empty"}},
		{"no nodes", "cluster: c\n", []string{"nodes: at least one node is required"}},
		{"empty nodes", "cluster: c\nnodes: []\n", []string{"nodes: at least one node is required"}},
		{"empty name", "cluster: c\nnodes:\n" + node("''", "http://a.example.com"), []string{"nodes[0].name"}},
		{"uppercase name", "cluster: c\nnodes:\n" + node("East", "http://a.example.com"), []string{"nodes[0].name", `"East"`}},
		{"leading dash", "cluster: c\nnodes:\n" + node("-a", "http://a.example.com"), []string{"nodes[0].name"}},
		{"long name", "cluster: c\nnodes:\n" + node(strings.Repeat("a", 65), "http://a.example.com"), []string{"nodes[0].name"}},
		{
			"duplicate name",
			"cluster: c\nnodes:\n" + node("a", "http://a.example.com") + node("b", "http://b.example.com") + node("a", "http://c.example.com"),
			[]string{`nodes[2].name: "a" is used more than once`},
		},
		{"missing url", "cluster: c\nnodes:\n  - name: a\n", []string{"nodes[0].url", "must be an absolute http or https URL"}},
		{"unparsable url", "cluster: c\nnodes:\n" + node("a", "'http://a b.example.com'"), []string{"nodes[0].url", "not a valid URL"}},
		{"relative url", "cluster: c\nnodes:\n" + node("a", "/api"), []string{"nodes[0].url", "absolute"}},
		{"ftp url", "cluster: c\nnodes:\n" + node("a", "ftp://a.example.com"), []string{"nodes[0].url", "http or https"}},
		{"no host", "cluster: c\nnodes:\n" + node("a", "http:///api"), []string{"nodes[0].url", "no host"}},
		{"port only host", "cluster: c\nnodes:\n" + node("a", "http://:8480/api"), []string{"nodes[0].url", "no host"}},
		{"query", "cluster: c\nnodes:\n" + node("a", "http://a.example.com/api?x=1"), []string{"nodes[0].url", "query"}},
		{"empty query", "cluster: c\nnodes:\n" + node("a", "http://a.example.com/api?"), []string{"nodes[0].url", "query"}},
		{"fragment", "cluster: c\nnodes:\n" + node("a", "http://a.example.com/api#top"), []string{"nodes[0].url", "fragment"}},
		{
			"every problem at once",
			"nodes:\n" + node("A", "ftp://a.example.com") + node("b", "http://b.example.com?x"),
			[]string{"cluster: must not be empty", "nodes[0].name", "nodes[0].url", "nodes[1].url"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewLoader(writeList(t, tc.list)).Nodes()
			if err == nil {
				t.Fatalf("Nodes succeeded: %+v", got)
			}
			if !reflect.DeepEqual(got, wire.Nodes{}) {
				t.Errorf("Nodes returned %+v with an error", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestNodesMissingFile(t *testing.T) {
	t.Parallel()
	_, err := NewLoader(filepath.Join(t.TempDir(), "nodes.yaml")).Nodes()
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Nodes of a missing file: %v, want os.ErrNotExist", err)
	}
}

func TestNodesUnreadableFile(t *testing.T) {
	t.Parallel()
	_, err := NewLoader(t.TempDir()).Nodes()
	if err == nil || !strings.Contains(err.Error(), "read node list") {
		t.Errorf("Nodes of a directory: %v, want a read error", err)
	}
}

func TestNodesCache(t *testing.T) {
	t.Parallel()
	// same has the same length as validList, so only the modification time can
	// tell the two apart.
	same := strings.Replace(validList, "example", "exampla", 1)
	path := writeList(t, validList)
	l := NewLoader(path)

	first, err := l.Nodes()
	if err != nil {
		t.Fatalf("first Nodes: %v", err)
	}
	if first.Cluster != "example" {
		t.Fatalf("cluster = %q", first.Cluster)
	}

	// Same size and modification time: the cached value is returned.
	rewrite(t, path, same, 0)
	got, err := l.Nodes()
	if err != nil || got.Cluster != "example" {
		t.Errorf("unchanged file: Nodes = %q, %v; want the cached example", got.Cluster, err)
	}

	// New modification time: the file is read again.
	rewrite(t, path, same, time.Second)
	got, err = l.Nodes()
	if err != nil || got.Cluster != "exampla" {
		t.Errorf("new mtime: Nodes = %q, %v; want exampla", got.Cluster, err)
	}

	// New size with the same modification time: the file is read again.
	rewrite(t, path, validList+"\n", time.Second)
	got, err = l.Nodes()
	if err != nil || got.Cluster != "example" {
		t.Errorf("new size: Nodes = %q, %v; want example", got.Cluster, err)
	}
}

func TestNodesFailedReloadIsNotCached(t *testing.T) {
	t.Parallel()
	bad := strings.Replace(validList, "name: east", "name: East", 1) // same length as validList
	path := writeList(t, validList)
	l := NewLoader(path)
	if _, err := l.Nodes(); err != nil {
		t.Fatalf("first Nodes: %v", err)
	}

	rewrite(t, path, bad, time.Second)
	if _, err := l.Nodes(); err == nil {
		t.Fatal("invalid file accepted")
	}
	// The failed load must not leave the previous value or the new file state
	// behind: the next call reads the file again, even with the same size and
	// modification time.
	rewrite(t, path, validList, time.Second)
	got, err := l.Nodes()
	if err != nil || got.Nodes[0].Name != "east" {
		t.Errorf("after fixing the file: Nodes = %+v, %v", got, err)
	}

	// A file that disappears is an error, and also drops the cache.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Nodes(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removed file: %v, want os.ErrNotExist", err)
	}
	rewrite(t, path, validList, time.Second)
	if got, err := l.Nodes(); err != nil || got.Cluster != "example" {
		t.Errorf("recreated file: Nodes = %+v, %v", got, err)
	}
}

func TestNodesResultIsACopy(t *testing.T) {
	t.Parallel()
	l := NewLoader(writeList(t, validList))
	first, err := l.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	first.Nodes[0].Name = "changed"
	first.Nodes = append(first.Nodes[:1], wire.Node{Name: "extra", URL: "http://x.example.com"})

	second, err := l.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if second.Nodes[0].Name != "east" || len(second.Nodes) != 2 || second.Nodes[1].Name != "west" {
		t.Errorf("cache was modified through a returned value: %+v", second)
	}
}

func TestNodesConcurrent(t *testing.T) {
	t.Parallel()
	path := writeList(t, validList)
	l := NewLoader(path)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got, err := l.Nodes()
				if err != nil || len(got.Nodes) != 2 {
					t.Errorf("goroutine %d: Nodes = %+v, %v", i, got, err)
					return
				}
				got.Nodes[0].Name = "scribble"
			}
		}()
	}
	wg.Wait()
}
