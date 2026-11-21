package main

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

type fakeInfo struct{ fs.FileInfo }

func (fakeInfo) Sys() any { return nil }

func TestOwnerOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := statOf(t, dir)
	if got, err := ownerOf(info); err != nil || got.uid != int(st.Uid) || got.gid != int(st.Gid) {
		t.Errorf("ownerOf = %+v, %v, want %d:%d", got, err, st.Uid, st.Gid)
	}
	if _, err := ownerOf(fakeInfo{info}); !errors.Is(err, errNoFolderOwner) {
		t.Errorf("ownerOf without owner = %v", err)
	}
}
