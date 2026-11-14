//go:build unix

package share

import (
	"os"

	"github.com/SpliceFS/server/internal/filestat"
)

// newDigestKey builds the cache key for the file at relPath from the file system's own
// description of it. A FileInfo that does not come from the file system carries no device,
// inode or change time, and its key is not identified, so nothing is cached for it.
func newDigestKey(relPath string, fileInfo os.FileInfo) digestKey {
	id, ok := filestat.Of(fileInfo)
	if !ok {
		return digestKey{}
	}
	return digestKey{
		path:       relPath,
		id:         id,
		size:       fileInfo.Size(),
		mtime:      fileInfo.ModTime().UnixNano(),
		identified: true,
	}
}
