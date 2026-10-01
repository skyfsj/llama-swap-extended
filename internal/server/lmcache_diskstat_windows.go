//go:build windows

package server

import (
	"golang.org/x/sys/windows"
)

// lmcacheDiskFreeSpace returns the free bytes available to the current user
// on the volume holding path (GetDiskFreeSpaceExW).
func lmcacheDiskFreeSpace(path string) (int64, error) {
	wpath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, free uint64
	if err := windows.GetDiskFreeSpaceExW(wpath, &freeToCaller, &total, &free); err != nil {
		return 0, err
	}
	return int64(freeToCaller), nil
}
