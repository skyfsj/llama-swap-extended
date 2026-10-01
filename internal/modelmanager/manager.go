// Package modelmanager provides a catalog of model weight files and a guarded
// single-file deletion primitive for filesystem-backed model sources.
package modelmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
	DefaultMaxFiles  = 2000
	DefaultMaxDepth  = 8
)

var modelFileSuffixes = []string{
	".gguf",
	".ggml",
	".safetensors",
	".bin",
	".pt",
	".pth",
	".ckpt",
	".onnx",
	".tflite",
	".flax",
	".msgpack",
	".h5",
	".weights",
}

var ErrNotFound = errors.New("model file not found")

// Manager scans the source configuration captured at construction time. A
// new manager is created when the process configuration is reloaded, which
// keeps one request from observing a partially changed source set.
type Manager struct {
	cfg config.ModelFilesConfig
}

// Source is a configured or automatically discovered model source.
type Source struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	Configured bool   `json:"configured"`
	Available  bool   `json:"available"`
	FileCount  int    `json:"file_count"`
	Error      string `json:"error,omitempty"`
}

// File is a model-looking regular file found in a source. Path is included so
// a user can copy it into a model command. Delete revalidates the source, ID,
// and path before it removes anything.
type File struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	RelativePath string    `json:"relative_path"`
	SourceID     string    `json:"source_id"`
	SourceType   string    `json:"source_type"`
	Repository   string    `json:"repository,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	Format       string    `json:"format"`
	Size         int64     `json:"size"`
	ModifiedAt   time.Time `json:"modified_at"`
	Symlink      bool      `json:"symlink,omitempty"`
	Registered   []string  `json:"registered_models,omitempty"`
	InUse        []string  `json:"in_use_models,omitempty"`
}

// Catalog is the response returned to the control API.
type Catalog struct {
	Data      []File    `json:"data"`
	Sources   []Source  `json:"sources"`
	Total     int       `json:"total"`
	Limit     int       `json:"limit"`
	Offset    int       `json:"offset"`
	Truncated bool      `json:"truncated"`
	ScannedAt time.Time `json:"scanned_at"`
}

// ListOptions controls a catalog query. Query filtering happens while
// scanning, so a large source does not need to be sent to the browser.
type ListOptions struct {
	SourceID string
	Query    string
	Limit    int
	Offset   int
}

type sourceSpec struct {
	source    Source
	typeName  string
	path      string
	recursive bool
}

// DownloadSource is a safe destination selected from the configured model
// sources. Individual-file sources are intentionally excluded because a
// repository download must have a directory root.
type DownloadSource struct {
	ID   string
	Type string
	Path string
}

// ResolveDownloadSource preserves the original Hugging Face preference for
// callers that do not choose a provider explicitly.
func (m *Manager) ResolveDownloadSource(sourceID string) (DownloadSource, error) {
	return m.ResolveDownloadSourceForProvider(sourceID, config.ModelFileSourceHFCache)
}

// ResolveDownloadSourceForProvider returns a safe configured or conventional
// cache destination. The provider-specific cache is preferred, with a generic
// directory as a compatibility fallback. A cache belonging to another
// provider is never selected implicitly.
func (m *Manager) ResolveDownloadSourceForProvider(sourceID, preferredType string) (DownloadSource, error) {
	if m == nil {
		return DownloadSource{}, errors.New("model file manager is nil")
	}
	requested := strings.TrimSpace(sourceID)
	preferredType = config.NormalizeModelFileSourceType(preferredType)
	if preferredType != config.ModelFileSourceHFCache && preferredType != config.ModelFileSourceMSCache {
		return DownloadSource{}, errors.New("download provider cache type is invalid")
	}
	var fallback *DownloadSource
	for _, spec := range m.sourceSpecs() {
		if spec.typeName == config.ModelFileSourceFile || spec.path == "" {
			continue
		}
		candidate := DownloadSource{ID: spec.source.ID, Type: spec.typeName, Path: spec.path}
		if err := rejectSymlinkPath(spec.path); err != nil {
			if requested == spec.source.ID {
				return DownloadSource{}, fmt.Errorf("download source %s: %w", spec.source.ID, err)
			}
			continue
		}
		if requested == spec.source.ID {
			if spec.typeName != preferredType && spec.typeName != config.ModelFileSourceDirectory {
				return DownloadSource{}, fmt.Errorf("%w: download source %q belongs to another provider", ErrNotFound, requested)
			}
			return candidate, nil
		}
		if requested == "" {
			if spec.typeName == preferredType {
				return candidate, nil
			}
			if fallback == nil && spec.typeName == config.ModelFileSourceDirectory {
				copy := candidate
				fallback = &copy
			}
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	if requested == "" {
		return DownloadSource{}, fmt.Errorf("%w: no directory or provider cache download destination is configured", ErrNotFound)
	}
	return DownloadSource{}, fmt.Errorf("%w: download source %q is unavailable or not a directory source", ErrNotFound, requested)
}

// New validates and snapshots a model-file configuration.
func New(cfg config.ModelFilesConfig) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg.Effective()}, nil
}

// List scans configured sources and returns a deterministic, paginated catalog.
// A missing optional source is represented in Sources with Available=false;
// one broken source therefore does not hide files from the other sources.
func (m *Manager) List(ctx context.Context, opts ListOptions) (Catalog, error) {
	if m == nil {
		return Catalog{}, errors.New("model file manager is nil")
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return Catalog{}, err
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultListLimit
	}
	if opts.Limit < 0 || opts.Limit > MaxListLimit {
		return Catalog{}, fmt.Errorf("model file list limit must be between 1 and %d", MaxListLimit)
	}
	if opts.Offset < 0 {
		return Catalog{}, errors.New("model file list offset must be >= 0")
	}

	query := strings.ToLower(strings.TrimSpace(opts.Query))
	allFiles := make([]File, 0)
	sources := make([]Source, 0)
	truncated := false
	for _, spec := range m.sourceSpecs() {
		if opts.SourceID != "" && spec.source.ID != opts.SourceID {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Catalog{}, err
		}

		files, sourceTruncated, err := m.scanSource(ctx, spec, query)
		source := spec.source
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Catalog{}, err
			}
			source.Error = err.Error()
			source.Available = false
		} else {
			source.Available = true
			source.FileCount = len(files)
		}
		sources = append(sources, source)
		allFiles = append(allFiles, files...)
		truncated = truncated || sourceTruncated
	}

	sort.SliceStable(allFiles, func(i, j int) bool {
		if allFiles[i].SourceID != allFiles[j].SourceID {
			return allFiles[i].SourceID < allFiles[j].SourceID
		}
		left := strings.ToLower(allFiles[i].RelativePath)
		right := strings.ToLower(allFiles[j].RelativePath)
		if left != right {
			return left < right
		}
		return allFiles[i].Path < allFiles[j].Path
	})

	total := len(allFiles)
	start := opts.Offset
	if start > total {
		start = total
	}
	end := start + opts.Limit
	if end > total {
		end = total
	}
	data := make([]File, end-start)
	copy(data, allFiles[start:end])

	return Catalog{
		Data:      data,
		Sources:   sources,
		Total:     total,
		Limit:     opts.Limit,
		Offset:    start,
		Truncated: truncated,
		ScannedAt: time.Now().UTC(),
	}, nil
}

// Delete removes one model file after re-validating that the path belongs to
// the selected source and that the supplied catalog ID still identifies it.
// Callers must perform any application-level usage/registration check before
// invoking this method. The manager itself never removes directories and does
// not follow a symlink as a deletion target.
func (m *Manager) Delete(ctx context.Context, sourceID, id, rawPath string) (File, error) {
	if m == nil {
		return File{}, errors.New("model file manager is nil")
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(rawPath) == "" {
		return File{}, fmt.Errorf("%w: source, id, and path are required", ErrNotFound)
	}

	target := cleanSourcePath(rawPath)
	if err := rejectSymlinkParents(target); err != nil {
		return File{}, fmt.Errorf("%w: target parent is unsafe: %v", ErrNotFound, err)
	}
	for _, spec := range m.sourceSpecs() {
		if spec.source.ID != sourceID || spec.path == "" {
			continue
		}
		file, ok, err := m.fileAtPath(spec, target)
		if err != nil {
			return File{}, err
		}
		if !ok || file.ID != id {
			return File{}, fmt.Errorf("%w: catalog entry does not match source path", ErrNotFound)
		}
		if err := os.Remove(target); err != nil {
			return File{}, fmt.Errorf("remove model file: %w", err)
		}
		return file, nil
	}
	return File{}, fmt.Errorf("%w: source %q is not configured", ErrNotFound, sourceID)
}

func (m *Manager) fileAtPath(spec sourceSpec, target string) (File, bool, error) {
	if err := rejectSymlinkPath(spec.path); err != nil {
		return File{}, false, fmt.Errorf("%w: source root is unsafe: %v", ErrNotFound, err)
	}
	if err := rejectSymlinkParents(target); err != nil {
		return File{}, false, fmt.Errorf("%w: target parent is unsafe: %v", ErrNotFound, err)
	}
	if !pathWithin(spec.path, target) {
		return File{}, false, fmt.Errorf("%w: path is outside the source", ErrNotFound)
	}
	if spec.typeName != config.ModelFileSourceFile {
		rootInfo, err := os.Lstat(spec.path)
		if err != nil {
			return File{}, false, fmt.Errorf("%w: source root is unavailable", ErrNotFound)
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || cleanSourcePath(spec.path) == target {
			return File{}, false, fmt.Errorf("%w: target is not a file below a directory source", ErrNotFound)
		}
	} else if rootInfo, err := os.Lstat(spec.path); err != nil || rootInfo.Mode()&os.ModeSymlink != 0 {
		return File{}, false, fmt.Errorf("%w: source root is not a real file", ErrNotFound)
	}
	lstat, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, false, fmt.Errorf("%w: path no longer exists", ErrNotFound)
		}
		return File{}, false, err
	}
	if lstat.IsDir() || !isModelFile(filepath.Base(target)) {
		return File{}, false, fmt.Errorf("%w: target is not a model file", ErrNotFound)
	}
	if spec.typeName != config.ModelFileSourceHFCache && lstat.Mode()&os.ModeSymlink != 0 {
		return File{}, false, fmt.Errorf("%w: directory sources do not delete symlinks", ErrNotFound)
	}
	if spec.typeName == config.ModelFileSourceFile && cleanSourcePath(spec.path) != target {
		return File{}, false, fmt.Errorf("%w: target is not the configured file", ErrNotFound)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return File{}, false, fmt.Errorf("%w: symlink target is unavailable", ErrNotFound)
		}
		if !pathWithin(spec.path, resolved) {
			return File{}, false, fmt.Errorf("%w: symlink target is outside the source", ErrNotFound)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return File{}, false, err
	}
	relative, err := filepath.Rel(spec.path, target)
	if err != nil {
		return File{}, false, err
	}
	if spec.typeName == config.ModelFileSourceFile {
		relative = filepath.Base(target)
	}
	file := makeFile(spec.source, target, relative, info, lstat.Mode()&os.ModeSymlink != 0)
	if spec.typeName == config.ModelFileSourceHFCache {
		parts := strings.Split(filepath.Clean(relative), string(os.PathSeparator))
		if len(parts) < 4 || !strings.HasPrefix(parts[0], "models--") || parts[1] != "snapshots" {
			return File{}, false, fmt.Errorf("%w: target is outside an HF snapshot", ErrNotFound)
		}
		file.Repository = decodeHFRepositoryID(parts[0])
		file.Revision = parts[2]
	} else if spec.typeName == config.ModelFileSourceMSCache {
		parts := strings.Split(filepath.Clean(relative), string(os.PathSeparator))
		if len(parts) < 5 || parts[0] != "models" || parts[2] != "snapshots" {
			return File{}, false, fmt.Errorf("%w: target is outside a ModelScope snapshot", ErrNotFound)
		}
		file.Repository = decodeModelScopeRepositoryID(parts[1])
		file.Revision = parts[3]
	}
	return file, true, nil
}

func pathWithin(root, target string) bool {
	root = resolvedSourcePath(root)
	target = resolvedSourcePath(target)
	if root == "" || target == "" {
		return false
	}
	if root == target {
		return true
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && relative != "."
}

func resolvedSourcePath(path string) string {
	path = cleanSourcePath(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return cleanSourcePath(resolved)
	}
	return path
}

// RegisteredModels returns model IDs whose command or environment references
// candidate. It is intentionally conservative: an ambiguous textual match
// blocks deletion rather than risking a configured model losing its weights.
func RegisteredModels(cfg config.Config, candidate string) []string {
	candidate = cleanSourcePath(candidate)
	if candidate == "" {
		return nil
	}
	registered := make([]string, 0)
	for modelID, model := range cfg.Models {
		if stringsContainPath(model.Cmd, candidate) {
			registered = append(registered, modelID)
			continue
		}
		backendArgsMatch := false
		for _, arg := range model.Backend.Arguments {
			if stringsContainPath(arg, candidate) {
				backendArgsMatch = true
				break
			}
		}
		if backendArgsMatch {
			registered = append(registered, modelID)
			continue
		}
		for _, value := range model.Env {
			if stringsContainPath(value, candidate) {
				registered = append(registered, modelID)
				break
			}
		}
	}
	sort.Strings(registered)
	return registered
}

func stringsContainPath(value, candidate string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	paths := []string{candidate}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		paths = append(paths, cleanSourcePath(resolved))
	}
	args, err := config.SanitizeCommand(value)
	if err == nil {
		for _, arg := range args {
			for _, path := range paths {
				if pathTokenMatches(arg, path) {
					return true
				}
			}
		}
	}
	for _, path := range paths {
		if strings.Contains(value, path) || strings.Contains(filepath.ToSlash(value), filepath.ToSlash(path)) {
			return true
		}
	}
	return false
}

func pathTokenMatches(token, candidate string) bool {
	token = strings.TrimSpace(token)
	if token == candidate || cleanSourcePath(token) == candidate {
		return true
	}
	for _, prefix := range []string{"--model=", "--model-path=", "-m="} {
		if strings.HasPrefix(token, prefix) && pathTokenMatches(strings.TrimPrefix(token, prefix), candidate) {
			return true
		}
	}
	if strings.ContainsRune(token, os.PathSeparator) || filepath.IsAbs(token) {
		candidatePath := cleanSourcePath(token)
		if info, err := os.Stat(candidatePath); err == nil && info.IsDir() && pathWithin(candidatePath, candidate) {
			return true
		}
	}
	return strings.Contains(token, candidate)
}

func (m *Manager) sourceSpecs() []sourceSpec {
	names := make([]string, 0, len(m.cfg.Sources))
	for name := range m.cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]sourceSpec, 0, len(names)+2)
	hasHFSource := false
	hasModelScopeSource := false
	usedIDs := make(map[string]struct{}, len(names)+2)
	for _, name := range names {
		sourceCfg := m.cfg.Sources[name]
		typeName := config.NormalizeModelFileSourceType(sourceCfg.Type)
		if typeName == "" {
			// New has already validated the config. Keep this guard so a Manager
			// cannot panic if it is ever constructed by a future deserializer.
			continue
		}
		if typeName == config.ModelFileSourceHFCache {
			hasHFSource = true
		} else if typeName == config.ModelFileSourceMSCache {
			hasModelScopeSource = true
		}
		path := cleanSourcePath(sourceCfg.Path)
		if typeName == config.ModelFileSourceHFCache && path == "" {
			// A download destination must be usable before the cache directory
			// exists. Listing still reports it as unavailable until it is
			// created, but the queue can create it on the first download.
			path = defaultHFCachePath()
		} else if typeName == config.ModelFileSourceMSCache && path == "" {
			path = defaultModelScopeCachePath()
		}
		recursive := true
		if sourceCfg.Recursive != nil {
			recursive = *sourceCfg.Recursive
		}
		specs = append(specs, sourceSpec{
			source: Source{
				ID:         name,
				Name:       name,
				Type:       typeName,
				Path:       path,
				Configured: true,
			},
			typeName:  typeName,
			path:      path,
			recursive: recursive,
		})
		usedIDs[name] = struct{}{}
	}

	// An explicit hf_cache source controls its own discovery behavior. When no
	// such source is configured, expose the conventional cache automatically so
	// the page is useful without requiring a config edit.
	if !hasHFSource {
		id := "hf-cache"
		if _, exists := usedIDs[id]; exists {
			id = "hf-cache-auto"
		}
		path := firstHFCachePath()
		if path == "" {
			path = defaultHFCachePath()
		}
		specs = append(specs, sourceSpec{
			source: Source{
				ID:         id,
				Name:       "Hugging Face cache",
				Type:       config.ModelFileSourceHFCache,
				Path:       path,
				Configured: false,
			},
			typeName: config.ModelFileSourceHFCache,
			path:     path,
		})
	}
	if !hasModelScopeSource {
		id := "modelscope-cache"
		if _, exists := usedIDs[id]; exists {
			id = "modelscope-cache-auto"
		}
		path := firstModelScopeCachePath()
		if path == "" {
			path = defaultModelScopeCachePath()
		}
		specs = append(specs, sourceSpec{
			source: Source{
				ID:         id,
				Name:       "ModelScope cache",
				Type:       config.ModelFileSourceMSCache,
				Path:       path,
				Configured: false,
			},
			typeName: config.ModelFileSourceMSCache,
			path:     path,
		})
	}
	return specs
}

func (m *Manager) scanSource(ctx context.Context, spec sourceSpec, query string) ([]File, bool, error) {
	ctx = normalizeContext(ctx)
	if spec.path == "" {
		return nil, false, fmt.Errorf("source path was not found")
	}
	if err := rejectSymlinkPath(spec.path); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(spec.path)
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("source path must not be a symlink")
	}
	if spec.typeName == config.ModelFileSourceFile {
		if !info.Mode().IsRegular() {
			return nil, false, errors.New("source path is not a regular file")
		}
		if !isModelFile(info.Name()) || !matchesQuery(info.Name(), info.Name(), query) {
			return []File{}, false, nil
		}
		return []File{makeFile(spec.source, spec.path, filepath.Base(spec.path), info, false)}, false, nil
	}
	if !info.IsDir() {
		return nil, false, errors.New("source path is not a directory")
	}

	maxFiles := m.cfg.MaxFiles
	maxDepth := m.cfg.MaxDepth
	if spec.typeName == config.ModelFileSourceHFCache {
		return scanHFCache(ctx, spec.source, spec.path, query, maxFiles, maxDepth)
	}
	if spec.typeName == config.ModelFileSourceMSCache {
		return scanModelScopeCache(ctx, spec.source, spec.path, query, maxFiles, maxDepth)
	}
	return scanDirectory(ctx, spec.source, spec.path, query, maxFiles, maxDepth, spec.recursive)
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func scanDirectory(ctx context.Context, source Source, root, query string, maxFiles, maxDepth int, recursive bool) ([]File, bool, error) {
	files := make([]File, 0)
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry == nil {
			return nil
		}
		depth := relativeDepth(root, path)
		if entry.IsDir() {
			if path != root && (!recursive || depth > maxDepth) {
				return fs.SkipDir
			}
			return nil
		}
		// Generic directory sources do not follow links. HF snapshot links are
		// handled by scanHFCache, where they are part of the cache contract.
		if entry.Type()&os.ModeSymlink != 0 || depth > maxDepth || !isModelFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !matchesQuery(entry.Name(), path, query) {
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, makeFile(source, path, relative, info, false))
		return nil
	})
	if err != nil {
		return nil, truncated, err
	}
	return files, truncated, nil
}

func relativeDepth(root, path string) int {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return 0
	}
	return len(strings.Split(filepath.Clean(relative), string(os.PathSeparator)))
}

func matchesQuery(name, path, query string) bool {
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), query) || strings.Contains(strings.ToLower(path), query)
}

func isModelFile(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range modelFileSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func modelFileFormat(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range modelFileSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimPrefix(suffix, ".")
		}
	}
	return "unknown"
}

func makeFile(source Source, path, relative string, info fs.FileInfo, symlink bool) File {
	relative = filepath.ToSlash(relative)
	sum := sha256.Sum256([]byte(source.ID + "\x00" + relative))
	return File{
		ID:           hex.EncodeToString(sum[:]),
		Name:         filepath.Base(path),
		Path:         path,
		RelativePath: relative,
		SourceID:     source.ID,
		SourceType:   source.Type,
		Format:       modelFileFormat(path),
		Size:         info.Size(),
		ModifiedAt:   info.ModTime().UTC(),
		Symlink:      symlink,
	}
}

func cleanSourcePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(strings.TrimSpace(path)))
	if err != nil {
		return filepath.Clean(strings.TrimSpace(path))
	}
	return filepath.Clean(abs)
}

func firstHFCachePath() string {
	for _, path := range discoverHFCachePaths() {
		return path
	}
	return ""
}

func defaultHFCachePath() string {
	for _, candidate := range hfCachePathCandidates() {
		if path := cleanSourcePath(candidate); path != "" {
			return path
		}
	}
	return ""
}

func discoverHFCachePaths() []string {
	candidates := hfCachePathCandidates()
	paths := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		path := cleanSourcePath(candidate)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			paths = append(paths, path)
		}
	}
	return paths
}

func hfCachePathCandidates() []string {
	candidates := make([]string, 0, 8)
	appendEnv := func(name string) {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			candidates = append(candidates, value)
		}
	}
	appendEnv("HF_HUB_CACHE")
	appendEnv("HUGGINGFACE_HUB_CACHE")
	appendEnv("TRANSFORMERS_CACHE")
	if home := strings.TrimSpace(os.Getenv("HF_HOME")); home != "" {
		candidates = append(candidates, filepath.Join(home, "hub"))
	}
	if cacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cacheHome != "" {
		candidates = append(candidates, filepath.Join(cacheHome, "huggingface", "hub"))
	}
	if cacheHome, err := os.UserCacheDir(); err == nil && cacheHome != "" {
		candidates = append(candidates, filepath.Join(cacheHome, "huggingface", "hub"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".cache", "huggingface", "hub"))
	}
	return candidates
}

func firstModelScopeCachePath() string {
	for _, path := range discoverModelScopeCachePaths() {
		return path
	}
	return ""
}

func defaultModelScopeCachePath() string {
	for _, candidate := range modelScopeCachePathCandidates() {
		if path := cleanSourcePath(candidate); path != "" {
			return path
		}
	}
	return ""
}

func discoverModelScopeCachePaths() []string {
	candidates := modelScopeCachePathCandidates()
	paths := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		path := cleanSourcePath(candidate)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			paths = append(paths, path)
		}
	}
	return paths
}

func modelScopeCachePathCandidates() []string {
	candidates := make([]string, 0, 4)
	if value := strings.TrimSpace(os.Getenv("MODELSCOPE_CACHE")); value != "" {
		candidates = append(candidates, value)
	}
	if cacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cacheHome != "" {
		candidates = append(candidates, filepath.Join(cacheHome, "modelscope"))
	}
	if cacheHome, err := os.UserCacheDir(); err == nil && cacheHome != "" {
		candidates = append(candidates, filepath.Join(cacheHome, "modelscope"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".cache", "modelscope"))
	}
	return candidates
}
