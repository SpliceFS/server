//go:build darwin

package filestat

import (
	"path/filepath"
	"testing"
)

// Every file system a Mac keeps files on (APFS, HFS+) records when a file was created.
func TestStatReportsTheBirthTime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeFile(t, path, "data")
	if got := mustStat(t, path); got.Btime == 0 {
		t.Errorf("Stat = %+v, want a birth time", got)
	}
}
