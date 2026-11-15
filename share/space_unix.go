//go:build unix

package share

import (
	"fmt"
	"os"
	"syscall"

	"github.com/SpliceFS/common/wire"
)

// openFlags opens read-only without blocking, so that a file replaced by a named
// pipe between the check and the open cannot hang the server.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// statSpace reports the total and the unprivileged free bytes of the file system
// holding path. The field types of syscall.Statfs_t differ between platforms, so
// every value goes through nonNegative.
func statSpace(path string) (wire.Space, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return wire.Space{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	block := nonNegative(stats.Bsize)
	return wire.Space{Total: nonNegative(stats.Blocks) * block, Free: nonNegative(stats.Bavail) * block}, nil
}

// nonNegative converts a size reported by the kernel to uint64. The kernel never
// reports a negative size, so a negative value becomes 0 and not a huge number.
func nonNegative[T ~int32 | ~uint32 | ~int64 | ~uint64](value T) uint64 {
	return uint64(max(value, 0))
}
