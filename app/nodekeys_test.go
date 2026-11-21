package app

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/SpliceFS/common/wire"
)

// The seed of the first test vector of RFC 8032 section 7.1 and its public key, and the private
// key of Alice in RFC 7748 section 6.1 and hers. They are fixed answers from the standards,
// so a test that uses them does not depend on how the code works out a public key.
const (
	rfcSignSeed   = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	rfcSignPublic = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	rfcEncryptKey = "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"
	rfcEncryptPub = "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"
)

// unhex returns the bytes of a hexadecimal text.
func unhex(t *testing.T, text string) []byte {
	t.Helper()
	data, err := hex.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// b64 is the form of a key in a file and in a node's announcement.
func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ownNode returns what the node list must say about the node that serves fix: its name and
// URL from the settings of the tests, and its id and public keys from the files that it saved.
func ownNode(t *testing.T, fix *fixture) wire.Node {
	t.Helper()
	idText, err := os.ReadFile(fix.data.nodeIDFile)
	if err != nil {
		t.Fatal(err)
	}
	id, err := parseNodeID(idText)
	if err != nil {
		t.Fatal(err)
	}
	keysText, err := os.ReadFile(fix.data.nodeKeysFile)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseNodeKeys(keysText)
	if err != nil {
		t.Fatal(err)
	}
	public := keys.public()
	return wire.Node{
		Name: "east", URL: "https://east.example.com/api",
		ID: id, SignKey: public.sign, EncryptKey: public.encrypt,
	}
}

// vectorRandom is the random source that makes the keys of the standards' vectors.
func vectorRandom(t *testing.T) *bytes.Reader {
	t.Helper()
	return bytes.NewReader(append(unhex(t, rfcSignSeed), unhex(t, rfcEncryptKey)...))
}

// newKeyPaths returns the data paths of a new state directory with its folders made.
func newKeyPaths(t *testing.T) dataPaths {
	t.Helper()
	paths := layout(t.TempDir())
	if err := paths.makeFolders(); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestNodeKeysAreMadeOnFirstStart(t *testing.T) {
	t.Parallel()
	paths := newKeyPaths(t)
	keys, err := paths.nodeKeys(vectorRandom(t))
	if err != nil {
		t.Fatal(err)
	}
	want := publicKeys{sign: b64(unhex(t, rfcSignPublic)), encrypt: b64(unhex(t, rfcEncryptPub))}
	if got := keys.public(); got != want {
		t.Errorf("public keys = %+v, want %+v", got, want)
	}
	wantFile := sprintKeys(t, rfcSignSeed, rfcEncryptKey)
	saved, err := os.ReadFile(paths.nodeKeysFile)
	if err != nil || string(saved) != wantFile {
		t.Errorf("saved keys = %q, %v, want %q", saved, err, wantFile)
	}
	info, err := os.Stat(paths.nodeKeysFile)
	if err != nil || info.Mode().Perm() != nodeKeysMode {
		t.Errorf("node keys file = %v, %v, want mode %o", info, err, nodeKeysMode)
	}
}

// A restart and a reload both run the same step again.
func TestNodeKeysAreKeptAndNeverReplaced(t *testing.T) {
	t.Parallel()
	paths := newKeyPaths(t)
	first, err := paths.nodeKeys(vectorRandom(t))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(paths.nodeKeysFile)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		// The random source of a later start would fail if it were used.
		again, againErr := paths.nodeKeys(iotest.ErrReader(errors.New("unused")))
		if againErr != nil {
			t.Fatal(againErr)
		}
		if first.public() != again.public() {
			t.Errorf("public keys changed: %+v, then %+v", first.public(), again.public())
		}
		if now, readErr := os.ReadFile(paths.nodeKeysFile); readErr != nil || !bytes.Equal(now, saved) {
			t.Errorf("node keys file = %q, %v, want it unchanged", now, readErr)
		}
	}
}

func TestNodeKeysOfTheStartThatSavedFirstWin(t *testing.T) {
	t.Parallel()
	paths := newKeyPaths(t)
	rival := keysRival{t: t, path: paths.nodeKeysFile, content: sprintKeys(t, rfcSignSeed, rfcEncryptKey)}
	// This start would make all-zero keys; the other start saved the vector keys first.
	keys, err := paths.nodeKeys(rival)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys.public().sign; got != b64(unhex(t, rfcSignPublic)) {
		t.Errorf("public signing key = %q, want the one of the keys saved first", got)
	}
}

// sprintKeys returns the content of a node keys file for two keys given in hexadecimal.
func sprintKeys(t *testing.T, signHex, encryptHex string) string {
	t.Helper()
	return "sign: " + b64(unhex(t, signHex)) + "\nencrypt: " + b64(unhex(t, encryptHex)) + "\n"
}

// keysRival plays another process that saves its node keys when this start asks for random
// bytes, as rival does for the node id.
type keysRival struct {
	t       *testing.T
	path    string
	content string
}

func (r keysRival) Read(p []byte) (int, error) {
	if err := os.WriteFile(r.path, []byte(r.content), nodeKeysMode); err != nil {
		r.t.Error(err)
	}
	clear(p)
	return len(p), nil
}

// A malformed file stops the start and is never replaced: a new key would be a different node.
func TestNodeKeysMalformedFileIsAnError(t *testing.T) {
	t.Parallel()
	good := b64(unhex(t, rfcSignSeed))
	short := b64(unhex(t, rfcSignSeed)[:31])
	tests := []struct {
		name    string
		content string
		want    string // part of the error, after the part every case has
	}{
		{"text that is not YAML", "sign: [\n", "yaml"},
		{"empty file", "", "sign"},
		{"sign is missing", "encrypt: " + good + "\n", "sign"},
		{"encrypt is missing", "sign: " + good + "\n", "encrypt"},
		{"sign is not base64", "sign: '***'\nencrypt: " + good + "\n", "sign"},
		{"encrypt is not base64", "sign: " + good + "\nencrypt: '***'\n", "encrypt"},
		{"sign is 31 bytes", "sign: " + short + "\nencrypt: " + good + "\n", "sign"},
		{"encrypt is 31 bytes", "sign: " + good + "\nencrypt: " + short + "\n", "encrypt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			paths := newKeyPaths(t)
			if err := os.WriteFile(paths.nodeKeysFile, []byte(tt.content), nodeKeysMode); err != nil {
				t.Fatal(err)
			}
			_, err := paths.nodeKeys(iotest.ErrReader(errors.New("unused")))
			if !errors.Is(err, ErrMalformedNodeKeys) || !strings.Contains(err.Error(), tt.want) ||
				!strings.Contains(err.Error(), paths.nodeKeysFile) {
				t.Errorf("nodeKeys = %v, want %v naming the file and %q", err, ErrMalformedNodeKeys, tt.want)
			}
			if now, readErr := os.ReadFile(paths.nodeKeysFile); readErr != nil || string(now) != tt.content {
				t.Errorf("file = %q, %v, want it unchanged", now, readErr)
			}
		})
	}
}

func TestNodeKeysErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		setup  func(t *testing.T, paths dataPaths)
		random func() io.Reader
		want   string
	}{
		{
			name:   "no randomness",
			random: func() io.Reader { return iotest.ErrReader(errors.New("injected")) },
			want:   "make node keys: read random bytes: injected",
		},
		{
			name:   "too little randomness",
			random: func() io.Reader { return bytes.NewReader(make([]byte, 63)) },
			want:   "make node keys: read random bytes: unexpected EOF",
		},
		{
			name: "path is a folder",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				if err := os.MkdirAll(paths.nodeKeysFile, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "read node keys file",
		},
		{
			// The folder is gone: reading says the file is missing and writing fails. A
			// read-only folder would not stop root, which runs the tests on Linux.
			name: "file cannot be written",
			setup: func(t *testing.T, paths dataPaths) {
				t.Helper()
				if err := os.RemoveAll(filepath.Dir(paths.nodeKeysFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: "write node keys file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			paths := newKeyPaths(t)
			if tt.setup != nil {
				tt.setup(t, paths)
			}
			random := io.Reader(zeros{})
			if tt.random != nil {
				random = tt.random()
			}
			_, err := paths.nodeKeys(random)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("nodeKeys = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}
