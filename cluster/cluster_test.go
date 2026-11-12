package cluster

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// nodeFile is the file name of node number n: a valid node id and the extension.
func nodeFile(n int) string { return fmt.Sprintf("%016x.yaml", n) }

// nodeContent is the content of a node file.
func nodeContent(name, url string) string { return "name: " + name + "\nurl: " + url + "\n" }

// put writes a node file with a modification time of epoch plus offset. The follower of the
// directory tells a changed file by size and time, and two writes in one test can land in
// the same clock tick, so the tests set the time themselves.
func put(t *testing.T, dir, file, content string, offset time.Duration) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch, epoch.Add(offset)); err != nil {
		t.Fatal(err)
	}
}

// dirNamedAsNodeFile returns a directory that holds a non-empty directory where the file of
// node 1 would be, so that the file can be neither read, replaced nor removed.
func dirNamedAsNodeFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	inner := filepath.Join(dir, nodeFile(1))
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, inner, "x", "", 0)
	return dir
}

// testLog returns a logger and the buffer that receives what it writes.
func testLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// load returns the nodes of l. Nodes reports no error even for bad files, which are only
// logged, so any error fails the test.
func load(t *testing.T, l *Loader) []wire.Node {
	t.Helper()
	got, err := l.Nodes()
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if got.Cluster != "" {
		t.Errorf("cluster = %q, want empty", got.Cluster)
	}
	return got.Nodes
}

// checkNodes fails the test when got is not want.
func checkNodes(t *testing.T, got, want []wire.Node) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("nodes = %+v, want %+v", got, want)
	}
}

func TestAnnounceWritesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	self := wire.Node{Name: "east", URL: "https://east.example.com/api"}
	if err := Announce(dir, "00000000000000e1", self); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	path := filepath.Join(dir, "00000000000000e1.yaml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := nodeContent("east", "https://east.example.com/api"); string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o644 {
		t.Errorf("mode = %v, want -rw-r--r--", info.Mode())
	}
	// The temporary file of the atomic write must not stay behind.
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("directory has %d entries (%v), want 1", len(entries), err)
	}
}

func TestAnnounceRewritesOnlyWhenContentDiffers(t *testing.T) {
	t.Parallel()
	east := wire.Node{Name: "east", URL: "https://east.example.com"}
	tests := []struct {
		name      string
		self      wire.Node
		want      string
		rewritten bool
	}{
		{"first announcement", east, nodeContent("east", "https://east.example.com"), true},
		{"same node again", east, nodeContent("east", "https://east.example.com"), false},
		{
			"new url", wire.Node{Name: "east", URL: "https://east.example.org"},
			nodeContent("east", "https://east.example.org"), true,
		},
		{
			"new name", wire.Node{Name: "west", URL: "https://east.example.org"},
			nodeContent("west", "https://east.example.org"), true,
		},
		{
			"same again after a change", wire.Node{Name: "west", URL: "https://east.example.org"},
			nodeContent("west", "https://east.example.org"), false,
		},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, nodeFile(1))
	// The steps share one file, so they run in order.
	for _, tc := range tests {
		// A rewrite gives the file the current time, which is told from epoch.
		if _, err := os.Stat(path); err == nil {
			if err := os.Chtimes(path, epoch, epoch); err != nil {
				t.Fatal(err)
			}
		}
		if err := Announce(dir, strings.TrimSuffix(nodeFile(1), ".yaml"), tc.self); err != nil {
			t.Fatalf("%s: Announce: %v", tc.name, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s: content = %q (%v), want %q", tc.name, got, err, tc.want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if rewritten := !info.ModTime().Equal(epoch); rewritten != tc.rewritten {
			t.Errorf("%s: rewritten = %v, want %v", tc.name, rewritten, tc.rewritten)
		}
	}
}

func TestAnnounceEmptyURLRemovesFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing bool
	}{
		{"file is removed", true},
		{"no file is not an error", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.existing {
				put(t, dir, nodeFile(1), nodeContent("east", "https://east.example.com"), 0)
			}
			other := nodeContent("west", "https://west.example.com")
			put(t, dir, nodeFile(2), other, 0)

			id := strings.TrimSuffix(nodeFile(1), ".yaml")
			if err := Announce(dir, id, wire.Node{Name: "east"}); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, nodeFile(1))); !os.IsNotExist(err) {
				t.Errorf("node file still there: %v", err)
			}
			// A node writes and removes only its own file.
			got, err := os.ReadFile(filepath.Join(dir, nodeFile(2)))
			if err != nil || string(got) != other {
				t.Errorf("file of another node = %q, %v", got, err)
			}
		})
	}
}

func TestAnnounceErrors(t *testing.T) {
	t.Parallel()
	self := wire.Node{Name: "east", URL: "https://east.example.com"}
	tests := []struct {
		name  string
		self  wire.Node
		setup func(t *testing.T) string
		want  string
	}{
		{
			"missing directory", self,
			func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			"write node file",
		},
		{
			"node file is a directory", self,
			dirNamedAsNodeFile,
			"write node file",
		},
		{
			"directory is not writable", self,
			func(t *testing.T) string {
				if os.Geteuid() == 0 {
					t.Skip("root can write to any directory")
				}
				dir := t.TempDir()
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				return dir
			},
			"write node file",
		},
		{
			"node file cannot be removed", wire.Node{Name: "east"},
			dirNamedAsNodeFile,
			"remove node file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := tc.setup(t)
			err := Announce(dir, strings.TrimSuffix(nodeFile(1), ".yaml"), tc.self)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Announce: %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLoaderEmptyDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{"empty directory", func(t *testing.T) string { return t.TempDir() }},
		{"missing directory", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "absent")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, buf := testLog()
			l := NewLoader(tc.dir(t), log)
			// The second call uses the cached empty list.
			for range 2 {
				if got := load(t, l); len(got) != 0 {
					t.Errorf("nodes = %+v, want none", got)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("unexpected log output: %s", buf)
			}
		})
	}
}

func TestLoaderSortsByNodeName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The order of the file names is not the order of the node names.
	put(t, dir, nodeFile(1), nodeContent("west", "http://west.example.com:8480/api/"), 0)
	put(t, dir, nodeFile(2), nodeContent("east", "https://east.example.com"), 0)
	put(t, dir, nodeFile(3), nodeContent("north", "https://north.example.com/"), 0)
	put(t, dir, nodeFile(4), nodeContent("east-2", "https://east2.example.com"), 0)
	log, buf := testLog()

	l := NewLoader(dir, log)
	want := []wire.Node{
		{Name: "east", URL: "https://east.example.com"},
		{Name: "east-2", URL: "https://east2.example.com"},
		{Name: "north", URL: "https://north.example.com"},
		{Name: "west", URL: "http://west.example.com:8480/api"},
	}
	checkNodes(t, load(t, l), want)
	checkNodes(t, load(t, l), want)
	if buf.Len() != 0 {
		t.Errorf("unexpected log output: %s", buf)
	}
}

func TestAnnounceThenLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	west := wire.Node{Name: "west", URL: "http://w/api/"}
	east := wire.Node{Name: "east", URL: "https://e"}
	if err := Announce(dir, "0000000000000001", west); err != nil {
		t.Fatal(err)
	}
	if err := Announce(dir, "0000000000000002", east); err != nil {
		t.Fatal(err)
	}
	log, buf := testLog()
	l := NewLoader(dir, log)
	checkNodes(t, load(t, l), []wire.Node{
		{Name: "east", URL: "https://e"}, {Name: "west", URL: "http://w/api"},
	})

	if err := Announce(dir, "0000000000000001", wire.Node{Name: "west"}); err != nil {
		t.Fatal(err)
	}
	checkNodes(t, load(t, l), []wire.Node{{Name: "east", URL: "https://e"}})
	if buf.Len() != 0 {
		t.Errorf("unexpected log output: %s", buf)
	}
}

func TestLoaderURLForms(t *testing.T) {
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
			dir := t.TempDir()
			put(t, dir, nodeFile(1), nodeContent("n", tc.in), 0)
			got := load(t, NewLoader(dir, slog.New(slog.DiscardHandler)))
			checkNodes(t, got, []wire.Node{{Name: "n", URL: tc.want}})
		})
	}
}

func TestLoaderIgnoresInvalidFile(t *testing.T) {
	t.Parallel()
	const okURL = "http://a.example.com"
	f9 := nodeFile(9)
	withURL := func(u string) string { return nodeContent("a", u) }
	withName := func(n string) string { return nodeContent(n, okURL) }
	tests := []struct {
		name    string
		file    string
		content string
		warn    string
	}{
		{"empty file", f9, "", "parse node file"},
		{"garbage", f9, "\x00\x01 not yaml: [", "parse node file"},
		{"syntax error", f9, "name: [", "parse node file"},
		{"unknown key", f9, withURL(okURL) + "extra: 1\n", "extra"},
		{"list instead of node", f9, "- name: a\n", "parse node file"},
		{"file name is not a node id", "east.yaml", withURL(okURL), "not a node id"},
		{"short node id", "abc.yaml", withURL(okURL), "not a node id"},
		{"upper case node id", "00000000000000E9.yaml", withURL(okURL), "not a node id"},
		{"no name", f9, "url: " + okURL + "\n", "must match"},
		{"empty name", f9, withName("''"), "must match"},
		{"upper case name", f9, withName("East"), "must match"},
		{"leading dash", f9, withName("-a"), "must match"},
		{"long name", f9, withName(strings.Repeat("a", 65)), "must match"},
		{"no url", f9, "name: a\n", "absolute http or https URL"},
		{"unparsable url", f9, withURL("'http://a b.example.com'"), "not a valid URL"},
		{"relative url", f9, withURL("/api"), "absolute http or https URL"},
		{"ftp url", f9, withURL("ftp://a.example.com"), "absolute http or https URL"},
		{"no host", f9, withURL("http:///api"), "no host"},
		{"port without host", f9, withURL("http://:8480/api"), "no host"},
		{"query", f9, withURL(okURL + "/api?x=1"), "query or a fragment"},
		{"empty query", f9, withURL(okURL + "/api?"), "query or a fragment"},
		{"fragment", f9, withURL(okURL + "/api#top"), "query or a fragment"},
	}
	good := wire.Node{Name: "good", URL: "https://good.example.com"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			put(t, dir, nodeFile(1), nodeContent(good.Name, good.URL), 0)
			put(t, dir, tc.file, tc.content, 0)
			log, buf := testLog()

			// The bad file never takes the good ones down with it.
			checkNodes(t, load(t, NewLoader(dir, log)), []wire.Node{good})
			out := buf.String()
			if !strings.Contains(out, tc.warn) || !strings.Contains(out, tc.file) {
				t.Errorf("log does not name %s and contain %q:\n%s", tc.file, tc.warn, out)
			}
		})
	}
}

func TestLoaderIgnoresForeignNames(t *testing.T) {
	t.Parallel()
	good := wire.Node{Name: "good", URL: "https://good.example.com"}
	valid := nodeContent("foreign", "https://foreign.example.com")
	for _, name := range []string{
		".tmp-123", ".0000000000000009.yaml", nodeFile(9) + ".tmp", "0000000000000009.yml",
		"0000000000000009.YAML", "0000000000000009", "README",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			put(t, dir, nodeFile(1), nodeContent(good.Name, good.URL), 0)
			put(t, dir, name, valid, 0)
			log, buf := testLog()
			checkNodes(t, load(t, NewLoader(dir, log)), []wire.Node{good})
			// Such files belong to other programs, so they are no reason for a warning.
			if buf.Len() != 0 {
				t.Errorf("unexpected log output: %s", buf)
			}
		})
	}

	t.Run("directory named like a node file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		put(t, dir, nodeFile(1), nodeContent(good.Name, good.URL), 0)
		if err := os.Mkdir(filepath.Join(dir, nodeFile(9)), 0o700); err != nil {
			t.Fatal(err)
		}
		put(t, filepath.Join(dir, nodeFile(9)), "inner.yaml", valid, 0)
		checkNodes(t, load(t, NewLoader(dir, slog.New(slog.DiscardHandler))), []wire.Node{good})
	})
}

func TestLoaderFollowsFileChanges(t *testing.T) {
	t.Parallel()
	east := nodeContent("east", "https://east.example.com")
	west := nodeContent("west", "https://west.example.com")
	nodeEast := wire.Node{Name: "east", URL: "https://east.example.com"}
	nodeWest := wire.Node{Name: "west", URL: "https://west.example.com"}
	nodeEast2 := wire.Node{Name: "east2", URL: "https://east2.example.com"}
	steps := []struct {
		name    string
		file    int // 0 means no change to the directory
		content string
		remove  bool
		offset  time.Duration
		want    []wire.Node
		warn    string
	}{
		{"first file", 1, east, false, 0, []wire.Node{nodeEast}, ""},
		{"second file", 2, west, false, 0, []wire.Node{nodeEast, nodeWest}, ""},
		{"nothing changed", 0, "", false, 0, []wire.Node{nodeEast, nodeWest}, ""},
		{
			"file turns bad, last good content stays", 1, "name: [\n", false, time.Second,
			[]wire.Node{nodeEast, nodeWest}, "parse node file",
		},
		{
			"bad file is not read again", 0, "", false, 0,
			[]wire.Node{nodeEast, nodeWest}, "",
		},
		{
			"fixed file is picked up", 1, nodeContent("east2", "https://east2.example.com"), false,
			2 * time.Second, []wire.Node{nodeEast2, nodeWest}, "",
		},
		{
			"file turns invalid, last good content stays", 2, nodeContent("west", "ftp://w"), false,
			time.Second, []wire.Node{nodeEast2, nodeWest}, "http or https",
		},
		{"deleted file disappears", 2, "", true, 0, []wire.Node{nodeEast2}, ""},
		{"last file deleted", 1, "", true, 0, nil, ""},
		{"file comes back", 1, east, false, 3 * time.Second, []wire.Node{nodeEast}, ""},
	}
	dir := t.TempDir()
	log, buf := testLog()
	l := NewLoader(dir, log)
	// The steps share one directory and one loader, so they run in order.
	for _, tc := range steps {
		buf.Reset()
		switch {
		case tc.file == 0:
		case tc.remove:
			if err := os.Remove(filepath.Join(dir, nodeFile(tc.file))); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		default:
			put(t, dir, nodeFile(tc.file), tc.content, tc.offset)
		}
		got, err := l.Nodes()
		if err != nil || !slices.Equal(got.Nodes, tc.want) {
			t.Errorf("%s: Nodes = %+v, %v; want %+v", tc.name, got.Nodes, err, tc.want)
		}
		if tc.warn == "" && buf.Len() != 0 {
			t.Errorf("%s: unexpected log output: %s", tc.name, buf)
		}
		if tc.warn != "" && !strings.Contains(buf.String(), tc.warn) {
			t.Errorf("%s: log does not contain %q:\n%s", tc.name, tc.warn, buf)
		}
	}
}

func TestLoaderDuplicateName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The first file by name wins, whatever the modification times are.
	put(t, dir, nodeFile(2), nodeContent("same", "http://two.example.com"), 0)
	put(t, dir, nodeFile(1), nodeContent("same", "http://one.example.com"), time.Hour)
	put(t, dir, nodeFile(3), nodeContent("other", "http://other.example.com"), 0)
	log, buf := testLog()
	l := NewLoader(dir, log)

	other := wire.Node{Name: "other", URL: "http://other.example.com"}
	checkNodes(t, load(t, l), []wire.Node{other, {Name: "same", URL: "http://one.example.com"}})
	out := buf.String()
	for _, want := range []string{
		"duplicate node name", "kept=" + nodeFile(1), "ignored=" + nodeFile(2),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}

	// The list is merged only when a file changed, so the warning is not repeated.
	buf.Reset()
	load(t, l)
	if buf.Len() != 0 {
		t.Errorf("warning repeated without a change: %s", buf)
	}

	// The second file takes over when the first one goes.
	if err := os.Remove(filepath.Join(dir, nodeFile(1))); err != nil {
		t.Fatal(err)
	}
	checkNodes(t, load(t, l), []wire.Node{other, {Name: "same", URL: "http://two.example.com"}})
}

func TestLoaderResultIsACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, nodeFile(1), nodeContent("east", "http://e.example.com"), 0)
	put(t, dir, nodeFile(2), nodeContent("west", "http://w.example.com"), 0)
	l := NewLoader(dir, slog.New(slog.DiscardHandler))
	first := load(t, l)
	first[0].Name = "changed"
	_ = append(first[:1], wire.Node{Name: "extra", URL: "http://x.example.com"})

	checkNodes(t, load(t, l), []wire.Node{
		{Name: "east", URL: "http://e.example.com"}, {Name: "west", URL: "http://w.example.com"},
	})
}

func TestLoaderConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, nodeFile(1), nodeContent("east", "http://e.example.com"), 0)
	put(t, dir, nodeFile(2), nodeContent("west", "http://w.example.com"), 0)
	l := NewLoader(dir, slog.New(slog.DiscardHandler))
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
