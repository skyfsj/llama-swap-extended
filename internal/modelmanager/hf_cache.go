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

func scanHFCache(ctx context.Context, source Source, root, query string, maxFiles, maxDepth int) ([]File, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, false, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, false, errors.New("HF cache root must be a real directory")
	}
	if err := rejectSymlinkPath(root); err != nil {
		return nil, false, err
	}
	repositories, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	files := make([]File, 0)
	truncated := false
	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return nil, truncated, err
		}
		if !repository.IsDir() || repository.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(repository.Name(), "models--") {
			continue
		}
		snapshots := filepath.Join(root, repository.Name(), "snapshots")
		snapshotsInfo, statErr := os.Lstat(snapshots)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, truncated, statErr
		}
		if snapshotsInfo.Mode()&os.ModeSymlink != 0 || !snapshotsInfo.IsDir() {
			continue
		}
		revisions, err := os.ReadDir(snapshots)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, truncated, err
		}
		repositoryID := decodeHFRepositoryID(repository.Name())
		for _, revision := range revisions {
			if err := ctx.Err(); err != nil {
				return nil, truncated, err
			}
			if !revision.IsDir() || revision.Type()&os.ModeSymlink != 0 {
				continue
			}
			snapshot := filepath.Join(snapshots, revision.Name())
			snapshotFiles, snapshotTruncated, err := scanHFSnapshot(ctx, source, root, snapshot, repositoryID, revision.Name(), query, maxFiles-len(files), maxDepth)
			if err != nil {
				return nil, truncated, err
			}
			files = append(files, snapshotFiles...)
			truncated = truncated || snapshotTruncated
			if truncated {
				return files, true, nil
			}
		}
	}
	return files, truncated, nil
}

func scanHFSnapshot(ctx context.Context, source Source, cacheRoot, snapshot, repository, revision, query string, maxFiles, maxDepth int) ([]File, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
		// Symlinks from snapshot files to blobs are the normal HF cache layout.
		// Stat follows the link to expose the actual weight size while Path keeps
		// the snapshot path that users can pass to an inference server.
		if depth > maxDepth || !isModelFile(entry.Name()) {
			return nil
		}
		var info fs.FileInfo
		var infoErr error
		if entry.Type()&os.ModeSymlink != 0 {
			info, infoErr = os.Stat(path)
		} else {
			info, infoErr = entry.Info()
		}
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		if !info.Mode().IsRegular() || !matchesQuery(entry.Name(), path, query) || !matchesQuery(repository, repository+"/"+path, query) {
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		relative, err := filepath.Rel(cacheRoot, path)
		if err != nil {
			return fmt.Errorf("relative model path: %w", err)
		}
		file := makeFile(source, path, relative, info, entry.Type()&os.ModeSymlink != 0)
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

func decodeHFRepositoryID(directoryName string) string {
	encoded := strings.TrimPrefix(directoryName, "models--")
	if encoded == "" {
		return directoryName
	}
	return strings.ReplaceAll(encoded, "--", "/")
}
