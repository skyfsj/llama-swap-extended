//go:build !windows

package server

import (
	"golang.org/x/sys/unix"
)

// lmcacheDiskFreeSpace returns the free bytes available to the current user
// on the filesystem holding path (statfs bavail × bsize).
func lmcacheDiskFreeSpace(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
