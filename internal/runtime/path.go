package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
)

// rejectSymlinkPath verifies every existing component of path without
// following links. Runtime state is written with atomic renames, but a
// symlinked parent would still redirect those writes outside the configured
// runtime root before the rename happens. Missing components are allowed so
// callers can create a new runtime tree after validating its nearest existing
// parent.
func rejectSymlinkPath(path string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil
	}

	current := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, current)
	if strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "\\") {
		current += string(filepath.Separator)
		rest = strings.TrimLeft(rest, "/\\")
	}
	for _, component := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Once an ancestor is missing, all later components are new
				// paths and cannot currently be symlinks.
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// macOS exposes a few standard roots through stable aliases (for
			// example /var -> /private/var). They are not operator-controlled
			// redirects and are required for ordinary temporary/test paths.
			if trustedDarwinSystemAlias(current) {
				continue
			}
			return fmt.Errorf("path component %s must not be a symlink", current)
		}
	}
	return nil
}

func trustedDarwinSystemAlias(path string) bool {
	if stdruntime.GOOS != "darwin" {
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
