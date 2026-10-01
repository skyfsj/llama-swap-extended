//go:build windows

package runtime

func withFileLock(path string, fn func() error) error {
	// Windows does not provide the Unix flock implementation used by the
	// manager, but the path boundary still matters for metadata writes. Keep
	// the same symlink refusal before executing the callback.
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	return fn()
}
