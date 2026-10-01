//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func validateStorePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("store.path must not be empty")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("store.path: %w", err)
	}

	if info, err := os.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("store.path: %w", err)
		}
		// File does not exist; ensure the parent directory is writable.
		dir := filepath.Dir(path)
		if err := unix.Access(dir, unix.W_OK); err != nil {
			return fmt.Errorf("store.path: directory %s is not writable: %w", dir, err)
		}
		return nil
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("store.path: %s is not a regular file", path)
	}

	// File exists; ensure it is writable.
	if err := unix.Access(path, unix.W_OK); err != nil {
		return fmt.Errorf("store.path: %s is not writable: %w", path, err)
	}
	return nil
}
