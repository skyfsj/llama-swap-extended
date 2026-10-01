package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// rejectSymlinkComponents verifies every existing component of path without
// following links. Store files are opened by the SQLite driver after config
// validation, so accepting a symlink here would let an otherwise in-bound
// store path redirect writes to an unrelated file or directory. Missing
// components are allowed; the caller still validates the existing parent
// directory's writability before startup.
func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil
	}
	current := filepath.VolumeName(clean)
	if rest := strings.TrimPrefix(clean, current); strings.HasPrefix(rest, string(filepath.Separator)) {
		current += string(filepath.Separator)
	}
	rest := strings.TrimPrefix(clean, current)
	for _, component := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Once an ancestor is missing, all later components are new
				// paths and cannot currently be symlinks. The caller will check
				// whether the nearest existing parent is writable.
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// macOS exposes a few standard roots through stable aliases (for
			// example /var -> /private/var). These are not user-controlled
			// redirects and are required for ordinary temporary/test paths.
			if trustedDarwinSystemAlias(current) {
				continue
			}
			return fmt.Errorf("path component %s is a symlink", current)
		}
	}
	return nil
}

func trustedDarwinSystemAlias(path string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	clean := filepath.Clean(path)
	if clean != "/var" && clean != "/tmp" && clean != "/etc" {
		return false
	}
	target, err := os.Readlink(clean)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(string(filepath.Separator), target)
	}
	return filepath.Clean(target) == filepath.Join("/private", clean)
}
