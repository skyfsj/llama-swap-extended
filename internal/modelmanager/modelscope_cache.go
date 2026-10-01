package modelmanager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// scanModelScopeCache follows the cache layout used by modelscope_hub:
// models/{namespace--repository}/snapshots/{revision}/...
func scanModelScopeCache(ctx context.Context, source Source, root, query string, maxFiles, maxDepth int) ([]File, bool, error) {
	ctx = normalizeContext(ctx)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, false, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, false, errors.New("ModelScope cache root must be a real directory")
	}
	if err := rejectSymlinkPath(root); err != nil {
		return nil, false, err
	}

	modelsRoot := filepath.Join(root, "models")
	modelsInfo, err := os.Lstat(modelsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return []File{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if modelsInfo.Mode()&os.ModeSymlink != 0 || !modelsInfo.IsDir() {
		return nil, false, errors.New("ModelScope cache models path must be a real directory")
	}

	repositories, err := os.ReadDir(modelsRoot)
	if err != nil {
		return nil, false, err
	}
	files := make([]File, 0)
	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !repository.IsDir() || repository.Type()&os.ModeSymlink != 0 {
			continue
		}
		repositoryID := decodeModelScopeRepositoryID(repository.Name())
		if repositoryID == "" {
			continue
		}
		snapshots := filepath.Join(modelsRoot, repository.Name(), "snapshots")
		snapshotsInfo, statErr := os.Lstat(snapshots)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, false, statErr
		}
		if snapshotsInfo.Mode()&os.ModeSymlink != 0 || !snapshotsInfo.IsDir() {
			continue
		}
		revisions, readErr := os.ReadDir(snapshots)
		if readErr != nil {
			return nil, false, readErr
		}
		for _, revision := range revisions {
			if !revision.IsDir() || revision.Type()&os.ModeSymlink != 0 {
				continue
			}
			snapshot := filepath.Join(snapshots, revision.Name())
			snapshotFiles, truncated, scanErr := scanModelScopeSnapshot(
				ctx, source, root, snapshot, repositoryID, revision.Name(), query, maxFiles-len(files), maxDepth,
			)
			if scanErr != nil {
				return nil, false, scanErr
			}
			files = append(files, snapshotFiles...)
			if truncated {
				return files, true, nil
			}
		}
	}
	return files, false, nil
}

func scanModelScopeSnapshot(ctx context.Context, source Source, cacheRoot, snapshot, repository, revision, query string, maxFiles, maxDepth int) ([]File, bool, error) {
	if maxFiles <= 0 {
		return []File{}, true, nil
	}
	files := make([]File, 0)
	truncated := false
	err := filepath.WalkDir(snapshot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry == nil {
			return nil
		}
		depth := relativeDepth(snapshot, path)
		if entry.IsDir() {
			if path != snapshot && depth > maxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || depth > maxDepth || !isModelFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		searchValue := repository + "/" + filepath.ToSlash(path)
		if !info.Mode().IsRegular() || (!matchesQuery(entry.Name(), path, query) && !matchesQuery(repository, searchValue, query)) {
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		relative, err := filepath.Rel(cacheRoot, path)
		if err != nil {
			return fmt.Errorf("relative ModelScope model path: %w", err)
		}
		file := makeFile(source, path, relative, info, false)
		file.Repository = repository
		file.Revision = revision
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, truncated, err
	}
	return files, truncated, nil
}

func decodeModelScopeRepositoryID(directoryName string) string {
	directoryName = strings.TrimSpace(directoryName)
	if directoryName == "" || !strings.Contains(directoryName, "--") {
		return ""
	}
	return strings.ReplaceAll(directoryName, "--", "/")
}
