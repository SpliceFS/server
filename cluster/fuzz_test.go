package cluster

import (
	"encoding/base64"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/internal/datadir"
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

// keySeeds are the starting inputs of FuzzPublicKey: a valid key and each way a key can be wrong.
func keySeeds() []string {
	return []string{
		signKey, encryptKey, "", "=", "AAAA", key(0), strings.TrimSuffix(signKey, "E=") + "F=",
		strings.TrimSuffix(signKey, "=") + "\n", signKey[:20] + "\r\n" + signKey[20:],
		strings.TrimSuffix(signKey, "=") + "A", signKey + signKey, "\x00",
		strings.ReplaceAll(strings.ReplaceAll(key(0xfb), "+", "-"), "/", "_"), key(0xfb), "A=======",
	}
}

// checkKeyForm fails the test when text is not the standard base64 of 32 bytes in its only
// spelling. A node publishes its keys to every client, which compares them as text, so a key
// that two spellings reach would look like two keys.
func checkKeyForm(t *testing.T, text string) {
	t.Helper()
	// Strict refuses the unused bits of the last character, and 44 characters leave no room
	// for the line breaks that the decoder skips.
	raw, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || len(raw) != 32 || len(text) != 44 {
		t.Fatalf("accepted key %q: %d bytes, %v", text, len(raw), err)
	}
}

// checkAcceptedKey fails the test when checkKey accepts text that is not a key.
func checkAcceptedKey(t *testing.T, text string) {
	t.Helper()
	if checkKey(text) == nil {
		checkKeyForm(t, text)
	}
}

func FuzzPublicKey(f *testing.F) {
	for _, seed := range keySeeds() {
		f.Add(seed)
	}
	f.Fuzz(checkAcceptedKey)
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
		nodeContent("a", "https://a") + keyLines(nodeID(1), signKey, encryptKey),
		nodeContent("a", "https://a") + keyLines(nodeID(2), signKey, encryptKey),
		nodeContent("a", "https://a") + keyLines(nodeID(1), signKey, ""),
		nodeContent("a", "https://a") + keyLines("", signKey, encryptKey),
		nodeContent("a", "https://a") + keyLines(nodeID(1), signKey, key(1)+"AAAA"),
		nodeContent("a", "https://a") + keyLines(nodeID(1), strings.TrimSuffix(signKey, "E=")+"F=", encryptKey),
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
		checkNodeKeys(t, filepath.Base(path), node, content)
	}
}

// checkNodeKeys fails the test when node, loaded from the file with the given name, has an id
// that is not the id in the name, or has keys without an id, or an id without two valid keys.
// Clients trust the keys of a listed node, so a file that does not hold all three or none must
// never be listed.
func checkNodeKeys(t *testing.T, fileName string, node wire.Node, content []byte) {
	t.Helper()
	if node.ID == "" {
		if node.SignKey != "" || node.EncryptKey != "" {
			t.Fatalf("keys without an id: %q", content)
		}
		return
	}
	if id, _ := datadir.IDOfFile(fileName); node.ID != id {
		t.Fatalf("accepted id %q in %s: %q", node.ID, fileName, content)
	}
	checkKeyForm(t, node.SignKey)
	checkKeyForm(t, node.EncryptKey)
}

func FuzzNodeFile(f *testing.F) {
	path := filepath.Join(f.TempDir(), nodeFile(1))
	for _, seed := range nodeFileSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, content []byte) { checkNodeFile(t, path, content) })
}
