package cluster

import (
	"bytes"
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// anotherKey makes a key other than testKey.
func anotherKey(t *testing.T) Key {
	t.Helper()
	random := append(bytes.Repeat([]byte{9}, ed25519.SeedSize), "qrstuvwxyz"...)
	made, err := NewKey(testNow.Add(time.Hour), bytes.NewReader(random))
	if err != nil {
		t.Fatal(err)
	}
	return made
}

// removeFile removes the file of the folder, as a sync tool does when the file is gone elsewhere.
func removeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestKeyReaderReadsTheKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := testKey(t)
	put(t, dir, KeyFile, string(MarshalKey(want)), 0)
	reader := NewKeyReader(dir, slog.New(slog.DiscardHandler))

	got, found := reader.Current()

	if !found || got.ID != want.ID || !bytes.Equal(got.Private, want.Private) {
		t.Errorf("Current = %+v, %v, want %+v", got, found, want)
	}
}

func TestKeyReaderHasNoKeyWithoutAFile(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]string{"empty folder": emptyDir(t), "no folder": missingDir(t)} {
		if got, found := NewKeyReader(dir, slog.New(slog.DiscardHandler)).Current(); found {
			t.Errorf("%s: Current = %+v, want none", name, got)
		}
	}
}

// The file is read again as it changes, so a key that a sync tool brings, replaces or removes
// takes effect at the next use with no signal.
func TestKeyReaderFollowsTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, second := testKey(t), anotherKey(t)
	reader := NewKeyReader(dir, slog.New(slog.DiscardHandler))
	if got, found := reader.Current(); found {
		t.Fatalf("before the file: Current = %+v, want none", got)
	}

	put(t, dir, KeyFile, string(MarshalKey(first)), time.Second)
	if got, found := reader.Current(); !found || got.ID != first.ID {
		t.Fatalf("after the file came: Current = %+v, %v, want %q", got, found, first.ID)
	}

	put(t, dir, KeyFile, string(MarshalKey(second)), 2*time.Second)
	if got, found := reader.Current(); !found || got.ID != second.ID {
		t.Fatalf("after the file changed: Current = %+v, %v, want %q", got, found, second.ID)
	}

	removeFile(t, dir, KeyFile)
	if got, found := reader.Current(); found {
		t.Errorf("after the file went: Current = %+v, want none", got)
	}
}

// A file that cannot be used never takes away the key that works: the last good key stays,
// with a warning that names the file and says nothing of what it holds.
func TestKeyReaderKeepsTheLastGoodKeyOfAFileThatBreaks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := testKey(t)
	put(t, dir, KeyFile, string(MarshalKey(good)), time.Second)
	log, logs := testLog()
	reader := NewKeyReader(dir, log)
	if _, found := reader.Current(); !found {
		t.Fatal("no key before the break")
	}

	put(t, dir, KeyFile, "id: "+good.ID+"\nprivate_key: "+secretText+"\npublic_key: x\n", 2*time.Second)
	got, found := reader.Current()

	if !found || got.ID != good.ID || !bytes.Equal(got.Private, good.Private) {
		t.Errorf("Current = %+v, %v, want the last good key %+v", got, found, good)
	}
	for _, want := range []string{"level=WARN", "keeping its last good content", "file=" + KeyFile, "private_key"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs, want)
		}
	}
	if strings.Contains(logs.String(), secretText) {
		t.Errorf("the log holds what the file holds: %s", logs)
	}
}

// secretText stands for a private key that a damaged file holds.
const secretText = "c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3JldDEy"

func TestKeyReaderHasNoKeyFromAFileThatNeverWorked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, KeyFile, "hello\n", 0)
	log, logs := testLog()

	got, found := NewKeyReader(dir, log).Current()

	if found {
		t.Errorf("Current = %+v, want none", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "file=key.yaml") {
		t.Errorf("log %q has no warning that names the file", logs)
	}
}

// The key of the cluster is the file key.yaml and no other: a stray file with a good key in it,
// as a sync tool or an editor leaves, is not the key.
func TestKeyReaderIgnoresOtherFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, "old-key.yaml", string(MarshalKey(anotherKey(t))), 0)
	log, logs := testLog()

	got, found := NewKeyReader(dir, log).Current()

	if found {
		t.Errorf("Current = %+v, want none", got)
	}
	if !strings.Contains(logs.String(), "file=old-key.yaml") || !strings.Contains(logs.String(), KeyFile) {
		t.Errorf("log %q has no warning that names the file and the name it must have", logs)
	}
}

func TestKeyReaderGivesACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := testKey(t)
	put(t, dir, KeyFile, string(MarshalKey(want)), 0)
	reader := NewKeyReader(dir, slog.New(slog.DiscardHandler))
	first, _ := reader.Current()
	clear(first.Private)

	got, found := reader.Current()

	if !found || !bytes.Equal(got.Private, want.Private) {
		t.Errorf("Current after the caller changed its copy = %+v, %v, want %+v", got, found, want)
	}
}

// Start, reload and the daily check ask at the same time, so the reader is safe for that.
func TestKeyReaderIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	put(t, dir, KeyFile, string(MarshalKey(testKey(t))), 0)
	reader := NewKeyReader(dir, slog.New(slog.DiscardHandler))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, found := reader.Current(); !found {
					t.Error("no key")
				}
			}
		})
	}
	wg.Wait()
}
