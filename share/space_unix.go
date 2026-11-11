//go:build unix

package share

import (
	"os"
	"syscall"

	"github.com/BlueRival/splicefs/internal/wire"
)

// openFlags opens read-only without blocking, so that a file replaced by a named
// pipe between the check and the open cannot hang the server.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// statSpace reports the total and the unprivileged free bytes of the file system
// holding path. The field types of syscall.Statfs_t differ between platforms, so
// every value is converted explicitly.
func statSpace(path string) (wire.Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return wire.Space{}, err
	}
	block := uint64(st.Bsize)
	total := uint64(st.Blocks) //nolint:unconvert // the field type differs between unix systems
	free := uint64(st.Bavail)  //nolint:unconvert // the field type differs between unix systems
	return wire.Space{Total: total * block, Free: free * block}, nil
}
