package modeldownload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
)

// rejectSymlinkPath verifies every existing component without following a
// link. Download destinations are writable model stores; a symlinked parent
// would let an apparently safe repository path escape the selected source.
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
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if trustedDarwinSystemAlias(current) {
				continue
			}
			return fmt.Errorf("path component %s must not be a symlink", current)
		}
	}
	return nil
}

// rejectSymlinkParents allows HF snapshot files themselves to be symlinks,
// but not any directory component leading to the target.
func rejectSymlinkParents(path string) error {
	return rejectSymlinkPath(filepath.Dir(filepath.Clean(path)))
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
