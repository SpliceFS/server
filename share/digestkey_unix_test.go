//go:build unix

package share

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

// statInfo is a file description that reports exactly the values a test sets.
type statInfo struct {
	size  int64
	mtime time.Time
	sys   any
}

func (statInfo) Name() string            { return "f" }
func (info statInfo) Size() int64        { return info.size }
func (statInfo) Mode() fs.FileMode       { return 0o644 }
func (info statInfo) ModTime() time.Time { return info.mtime }
func (statInfo) IsDir() bool             { return false }
func (info statInfo) Sys() any           { return info.sys }

// keyInput is everything a digest key is made from.
type keyInput struct {
	relPath string
	info    statInfo
	stat    syscall.Stat_t
}

func (input *keyInput) key() digestKey {
	input.info.sys = &input.stat
	return newDigestKey(input.relPath, input.info)
}

func baseKeyInput() *keyInput {
	input := &keyInput{relPath: "f", info: statInfo{size: 5, mtime: epoch()}}
	input.stat.Dev = 1
	input.stat.Ino = 2
	setChangeTime(&input.stat, 3*int64(time.Second))
	return input
}

func TestDigestKeyIsMadeOfEveryDetailOfTheFile(t *testing.T) {
	t.Parallel()
	base := baseKeyInput().key()
	if again := baseKeyInput().key(); !base.identified || base != again {
		t.Fatalf("the key of an unchanged file is %+v, then %+v", base, again)
	}
	changes := map[string]func(*keyInput){
		"path":                  func(input *keyInput) { input.relPath = "g" },
		"device":                func(input *keyInput) { input.stat.Dev = 7 },
		"inode":                 func(input *keyInput) { input.stat.Ino = 7 },
		"change time":           func(input *keyInput) { setChangeTime(&input.stat, 3*int64(time.Second)+1) },
		"size":                  func(input *keyInput) { input.info.size++ },
		"modification time":     func(input *keyInput) { input.info.mtime = input.info.mtime.Add(time.Nanosecond) },
		"whole second of mtime": func(input *keyInput) { input.info.mtime = input.info.mtime.Add(time.Second) },
	}
	for name, change := range changes {
		input := baseKeyInput()
		change(input)
		if input.key() == base {
			t.Errorf("a different %s gives the same key", name)
		}
	}
}

func TestDigestKeyWithoutFileSystemDetailsIsNotIdentified(t *testing.T) {
	t.Parallel()
	for name, sys := range map[string]any{"none": nil, "other type": "not a stat"} {
		if key := newDigestKey("f", statInfo{size: 5, mtime: epoch(), sys: sys}); key.identified {
			t.Errorf("sys %s: key %+v is identified", name, key)
		}
	}
}

func TestAsUint64KeepsDifferentNumbersDifferent(t *testing.T) {
	t.Parallel()
	if asUint64(int32(-1)) == asUint64(int32(0)) || asUint64(int32(-1)) == asUint64(int32(1)) {
		t.Error("a negative device number equals another one")
	}
	if got := asUint64(uint32(7)); got != 7 {
		t.Errorf("asUint64(7) = %d", got)
	}
}
