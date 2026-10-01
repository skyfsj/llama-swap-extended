package extensions

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Extension sources are managed as a tree of small text files. The entrypoint
// is always index.js at the root; other modules are reachable through relative
// imports. Files that a user added by hand but never submitted through the API
// are removed on the next save, so a file has to be editable and deletable from
// the same place for the two to stay consistent.
const (
	// EntryFile is the required entrypoint of every extension.
	EntryFile = "index.js"
	// ManifestFile is written by the manager from the manifest struct and is
	// deliberately not editable as a tree file.
	ManifestFile = "manifest.yaml"
	// MaxExtensionFiles counts the files one extension may submit at once.
	MaxExtensionFiles = 200
	// MaxExtensionFileBytes caps a single file.
	MaxExtensionFileBytes = 256 << 10
	// MaxExtensionTreeBytes caps the whole tree; the bundle it produces is
	// bounded separately by the 4 MiB limit in compileDirectory.
	MaxExtensionTreeBytes = 4 << 20
	// MaxExtensionPathLength is the longest accepted relative path.
	MaxExtensionPathLength = 160
)

// managedExtensions is the set of file extensions the API manages. Anything
// else found on disk is invisible to the editor and is removed by the next
// save, which keeps "what the UI shows" and "what gets written" the same set.
var managedExtensions = map[string]bool{
	".js":   true,
	".mjs":  true,
	".cjs":  true,
	".json": true,
	".md":   true,
	".txt":  true,
	".yaml": true,
	".yml":  true,
}

// normalizeExtensionPath validates one tree path and returns it cleaned. It is
// the single gate every path passes through, on write and on read.
func normalizeExtensionPath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("extension file path is empty")
	}
	if len(raw) > MaxExtensionPathLength {
		return "", fmt.Errorf("extension file path %q is too long", raw)
	}
	if strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("extension file path %q must use forward slashes", raw)
	}
	if path.IsAbs(raw) || filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("extension file path %q must be relative", raw)
	}
	cleaned := path.Clean(strings.TrimPrefix(raw, "./"))
	if cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("extension file path %q is not a file", raw)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return "", fmt.Errorf("extension file path %q is not relative", raw)
		case strings.HasPrefix(segment, "."):
			return "", fmt.Errorf("extension file path %q must not start with a dot", raw)
		case segment == "node_modules":
			return "", fmt.Errorf("extension file path %q must not be inside node_modules", raw)
		}
	}
	if !managedExtensions[path.Ext(cleaned)] {
		return "", fmt.Errorf("extension file %q has an unsupported extension %q", raw, path.Ext(cleaned))
	}
	return cleaned, nil
}

// validateFileMap checks a submitted tree before anything touches the disk.
func validateFileMap(files map[string]string) error {
	if len(files) == 0 {
		return errors.New("extension must contain at least one file")
	}
	if len(files) > MaxExtensionFiles {
		return fmt.Errorf("extension may not contain more than %d files", MaxExtensionFiles)
	}
	seen := map[string]string{}
	total := 0
	for name, data := range files {
		cleaned, err := normalizeExtensionPath(name)
		if err != nil {
			return err
		}
		if other, duplicate := seen[cleaned]; duplicate {
			return fmt.Errorf("extension file %q and %q are the same path", other, name)
		}
		seen[cleaned] = name
		if cleaned == ManifestFile {
			return fmt.Errorf("extension file %q is written by the manager", name)
		}
		if len(data) > MaxExtensionFileBytes {
			return fmt.Errorf("extension file %q exceeds %d KiB", name, MaxExtensionFileBytes>>10)
		}
		total += len(data)
	}
	if total > MaxExtensionTreeBytes {
		return fmt.Errorf("extension files exceed %d MiB", MaxExtensionTreeBytes>>20)
	}
	if _, ok := seen[EntryFile]; !ok {
		return fmt.Errorf("extension must contain %s", EntryFile)
	}
	if _, hasPackage := seen["package.json"]; hasPackage {
		if _, hasLock := seen["package-lock.json"]; !hasLock {
			return errors.New("package-lock.json is required")
		}
	}
	return nil
}

// stripManifest removes the manager-owned manifest from a submitted tree.
func stripManifest(files map[string]string) map[string]string {
	result := make(map[string]string, len(files))
	for name, data := range files {
		if name == ManifestFile {
			continue
		}
		result[name] = data
	}
	return result
}

// readTree loads every managed file below root, keyed by relative path.
func readTree(root string) (map[string]string, error) {
	files := map[string]string{}
	total := 0
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if current == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		relative, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if !managedExtensions[path.Ext(relative)] {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		if info.Size() > MaxExtensionFileBytes {
			return fmt.Errorf("extension file %q exceeds %d KiB", relative, MaxExtensionFileBytes>>10)
		}
		total += int(info.Size())
		if total > MaxExtensionTreeBytes {
			return fmt.Errorf("extension files exceed %d MiB", MaxExtensionTreeBytes>>20)
		}
		data, readErr := os.ReadFile(current)
		if readErr != nil {
			return readErr
		}
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// treeStamp summarizes the shape of a tree for change detection without
// hashing contents: the watcher polls every two seconds and reloads only what
// actually changed.
func treeStamp(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err.Error()
	}
	var parts []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
			continue
		}
		child := filepath.Join(root, entry.Name())
		_ = filepath.WalkDir(child, func(current string, item os.DirEntry, walkErr error) error {
			if walkErr != nil || item.IsDir() {
				return walkErr
			}
			relative, relErr := filepath.Rel(child, current)
			if relErr != nil {
				return relErr
			}
			relative = filepath.ToSlash(relative)
			if !managedExtensions[path.Ext(relative)] || strings.HasPrefix(relative, ".") {
				return nil
			}
			info, statErr := item.Info()
			if statErr != nil {
				return nil
			}
			parts = append(parts, fmt.Sprintf("%s/%s:%d:%d", entry.Name(), relative, info.Size(), info.ModTime().UnixNano()))
			return nil
		})
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// writeTree materializes a submitted tree below dst.
func writeTree(dst string, files map[string]string) error {
	for name, data := range files {
		cleaned, err := normalizeExtensionPath(name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(cleaned))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(data), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// removeFilesNotIn deletes every managed file below root that is not listed in
// keep. It is what makes "delete a file in the editor" stick.
// removeFilesNotIn deletes every managed file below root that is not listed in
// keep, then every directory that is neither kept as a declared empty
// directory nor an ancestor of a kept entry.
func removeFilesNotIn(root string, keep map[string]struct{}, keepDirectories []string) error {
	files, err := readTree(root)
	if err != nil {
		return err
	}
	emptied := map[string]bool{}
	for name := range files {
		if name == ManifestFile {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for parent := filepath.Dir(target); parent != root && !emptied[parent]; parent = filepath.Dir(parent) {
			emptied[parent] = true
			// A directory that only held deleted files should not linger as an
			// empty shell in the tree — unless the definition declares it.
			relative, relErr := filepath.Rel(root, parent)
			kept := relErr == nil && slices.Contains(keepDirectories, filepath.ToSlash(relative))
			if kept {
				break
			}
			if removeErr := os.Remove(parent); removeErr != nil {
				break
			}
		}
	}
	// Conversely, a declared directory that the previous tree never had is
	// created here so removes cannot erase it.
	for _, directory := range keepDirectories {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// sortedTreePaths returns the tree paths in a stable order.
func sortedTreePaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

// listEmptyDirectories walks root and returns managed directories that
// contain no managed files (relative, slash-separated, sorted). The manifest
// and dot/node_modules entries are skipped like readTree does.
func listEmptyDirectories(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || current == root {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			return filepath.SkipDir
		}
		relative, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		entries, readErr := os.ReadDir(current)
		if readErr != nil {
			return readErr
		}
		hasManagedContent := false
		for _, child := range entries {
			childName := child.Name()
			if strings.HasPrefix(childName, ".") || childName == "node_modules" {
				continue
			}
			if child.IsDir() {
				hasManagedContent = true
				break
			}
			if managedExtensions[path.Ext(childName)] {
				hasManagedContent = true
				break
			}
		}
		if !hasManagedContent {
			found = append(found, relative)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// normalizeDirectoryEntry validates one empty-directory entry from
// Definition.Directories: the same rules as a file path, minus the extension
// whitelist, with no trailing slash.
func normalizeDirectoryEntry(raw string) (string, error) {
	cleaned := strings.TrimSuffix(raw, "/")
	if cleaned == "" {
		return "", errors.New("extension directory path is empty")
	}
	if len(cleaned) > MaxExtensionPathLength {
		return "", fmt.Errorf("extension directory path %q is too long", cleaned)
	}
	if strings.ContainsRune(cleaned, 0) || strings.ContainsRune(cleaned, '\\') {
		return "", fmt.Errorf("extension directory path %q must use forward slashes", cleaned)
	}
	if path.IsAbs(cleaned) || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("extension directory path %q must be relative", cleaned)
	}
	cleaned = path.Clean(strings.TrimPrefix(cleaned, "./"))
	if cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("extension directory path %q is not a directory", raw)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return "", fmt.Errorf("extension directory path %q is not relative", raw)
		case strings.HasPrefix(segment, "."):
			return "", fmt.Errorf("extension directory path %q must not start with a dot", raw)
		case segment == "node_modules":
			return "", fmt.Errorf("extension directory path %q must not be inside node_modules", raw)
		}
	}
	return cleaned, nil
}

// validateDirectoryEntries normalizes and dedupes the directory list, and
// rejects a directory that collides with a file path.
func validateDirectoryEntries(files map[string]string, raw []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		cleaned, err := normalizeDirectoryEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, isFile := files[cleaned]; isFile {
			return nil, fmt.Errorf("extension directory %q is also a file", cleaned)
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	sort.Strings(out)
	return out, nil
}
