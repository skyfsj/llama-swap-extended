//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

func withFileLock(path string, fn func() error) error {
	// The lock file lives inside the manager-owned runtime root.  Do not follow
	// a symlink here: an operator or compromised process could otherwise point
	// the lock at an arbitrary path and make the supposedly atomic metadata
	// transaction depend on an external filesystem boundary.
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
