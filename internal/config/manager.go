package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigSource describes one YAML source and its write capability.
type ConfigSource struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Managed  bool   `json:"managed"`
}

type ConfigSnapshot struct {
	Config          json.RawMessage   `json:"config,omitempty"`
	YAML            string            `json:"yaml"`
	ETag            string            `json:"etag"`
	Sources         []ConfigSource    `json:"sources"`
	Ownership       map[string]string `json:"ownership,omitempty"`
	Writable        bool              `json:"writable"`
	RestartRequired bool              `json:"restartRequired"`
	RestartPaths    []string          `json:"restartPaths,omitempty"`
}

// SourceFiles is the complete source-aware view used by the settings center.
// Data contains exact bytes keyed by absolute source path; a missing managed
// overlay is represented by an empty byte slice only when the caller includes
// it in a subsequent update.
type SourceFiles struct {
	Sources []ConfigSource    `json:"sources"`
	Data    map[string][]byte `json:"-"`
	ETag    string            `json:"etag"`
}

type PatchIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type PatchValidation struct {
	Valid           bool         `json:"valid"`
	Issues          []PatchIssue `json:"issues,omitempty"`
	Diff            []PatchOp    `json:"diff,omitempty"`
	ApplyMode       string       `json:"applyMode"`
	RestartRequired bool         `json:"restartRequired"`
	RestartPaths    []string     `json:"restartPaths,omitempty"`
	// Pruned lists the routing locations a model removal adjusted, e.g.
	// "routing.router.settings.matrix.vars". It is empty when the patch removed
	// no models or left no dangling references behind.
	Pruned []string `json:"pruned,omitempty"`
}

// validationPathRe matches the leading dotted configuration path that the
// loader prefixes onto field-level validation errors, for example the
// "modelFiles" in "modelFiles.maxFiles must be between 0 and 100000". It
// requires at least one dot so section-level messages ("modelFiles.sources
// cannot contain an empty name") are left without a field path.
var validationPathRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z_][A-Za-z0-9_-]*)+)`)

// validationPathFromError converts a leading dotted configuration path in a
// loader error message into the slash-delimited form used by patch and
// settings paths, e.g. "/modelFiles/maxFiles". It returns "" when the message
// does not start with a field path.
func validationPathFromError(err error) string {
	if err == nil {
		return ""
	}
	match := validationPathRe.FindStringSubmatch(err.Error())
	if len(match) < 2 {
		return ""
	}
	return "/" + strings.ReplaceAll(match[1], ".", "/")
}

// YAMLValidation describes syntax and semantic validation for a user-edited
// configuration document. It intentionally contains no patch operations: the
// control plane presents the YAML document itself and keeps implementation
// details such as RFC 6902 private to the server.
type YAMLValidation struct {
	Valid  bool         `json:"valid"`
	Issues []PatchIssue `json:"issues,omitempty"`
}

type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
	From  string `json:"from,omitempty"`

	// valueSet/fromSet distinguish an explicit JSON null or empty "from" from
	// an omitted member. They are populated by decodePatch and intentionally
	// remain private so callers can continue constructing PatchOp literals.
	valueSet bool
	fromSet  bool
}

// MarshalJSON keeps explicit null values visible in validation diffs while
// retaining the compact shape used by existing callers.
func (op PatchOp) MarshalJSON() ([]byte, error) {
	valueSet := op.valueSet || op.Value != nil
	fromSet := op.fromSet || op.From != ""
	value := map[string]any{"op": op.Op, "path": op.Path}
	if valueSet {
		value["value"] = op.Value
	}
	if fromSet {
		value["from"] = op.From
	}
	return json.Marshal(value)
}

type ConfigManager struct {
	mu          sync.Mutex
	configPath  string
	configDir   string
	managedPath string
	reload      func(Config) error

	// applyGen counts committed ApplySources batches. A batch whose reload
	// callback failed rolls its file writes back only if no other batch
	// committed in the meantime — otherwise the rollback would clobber a
	// newer, already-committed configuration.
	applyGen uint64
}

func NewConfigManager(configPath, configDir string) (*ConfigManager, error) {
	if configPath == "" && configDir == "" {
		return nil, errors.New("config manager requires config path or directory")
	}
	absConfig, err := absOptional(configPath)
	if err != nil {
		return nil, err
	}
	absDir, err := absOptional(configDir)
	if err != nil {
		return nil, err
	}
	if err := validateConfigManagerPath(absConfig, false); err != nil {
		return nil, err
	}
	if err := validateConfigManagerPath(absDir, true); err != nil {
		return nil, err
	}
	managedBase := absDir
	if managedBase == "" {
		managedBase = filepath.Dir(absConfig)
	}
	return &ConfigManager{configPath: absConfig, configDir: absDir, managedPath: filepath.Join(managedBase, "90-llama-swap-managed.yaml")}, nil
}

// validateConfigManagerPath rejects symlinked configuration roots. The
// manager performs atomic renames and source ownership decisions; following a
// symlink here could otherwise make an apparently local edit replace an
// unrelated file outside the configured boundary.
func validateConfigManagerPath(path string, directory bool) error {
	if path == "" {
		return nil
	}
	// Check every existing component, not only the final path.  The manager
	// creates the managed overlay and performs atomic renames below this root;
	// a symlinked parent would otherwise redirect those writes outside the
	// configured configuration boundary.  Missing components remain allowed so
	// callers can point at a not-yet-created config file/directory.
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("config manager path %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config manager path %s must not be a symlink", path)
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("config manager path %s is not a directory", path)
	}
	if !directory && info.IsDir() {
		return fmt.Errorf("config manager path %s is a directory", path)
	}
	return nil
}

func absOptional(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func (m *ConfigManager) SetReload(fn func(Config) error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reload = fn
	m.mu.Unlock()
}

func (m *ConfigManager) sources() ([]ConfigSource, error) {
	if m.configDir != "" {
		if err := rejectSymlinkComponents(m.configDir); err != nil {
			return nil, fmt.Errorf("config directory %s: %w", m.configDir, err)
		}
	}
	if m.configPath != "" {
		if err := rejectSymlinkComponents(m.configPath); err != nil {
			return nil, fmt.Errorf("config path %s: %w", m.configPath, err)
		}
	}
	var paths []string
	if m.configPath != "" {
		paths = append(paths, m.configPath)
	}
	if m.configDir != "" {
		files, err := listYAMLFiles(m.configDir)
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			if path != m.configPath {
				paths = append(paths, path)
			}
		}
	}
	seen := map[string]bool{}
	out := make([]ConfigSource, 0, len(paths)+1)
	for _, path := range paths {
		if err := rejectSymlinkComponents(path); err != nil {
			return nil, fmt.Errorf("config source %s: %w", path, err)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("config source %s must not be a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("config source %s must be a regular file", path)
		}
		out = append(out, ConfigSource{Path: path, Writable: info.Mode().Perm()&0200 != 0, Managed: path == m.managedPath})
	}
	// A config directory always has a writable managed overlay as the target
	// for newly-created identity entities, even before the first patch creates
	// the file. Advertise that destination alongside existing sources so the
	// control plane can accurately report where a new model/profile/peer will
	// be written and whether a read-only base still has an available overlay.
	if m.configDir != "" && m.managedPath != "" && !seen[m.managedPath] {
		info, err := os.Lstat(m.managedPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			out = append(out, ConfigSource{Path: m.managedPath, Writable: true, Managed: true})
		} else {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("config source %s must not be a symlink", m.managedPath)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("config source %s must be a regular file", m.managedPath)
			}
			out = append(out, ConfigSource{Path: m.managedPath, Writable: info.Mode().Perm()&0200 != 0, Managed: true})
		}
	}
	if len(out) == 0 {
		// A config-path-only manager writes new entities back to that single
		// source, including when the file has not been created yet. Advertising
		// the managed overlay here would produce a misleading source/ETag and
		// make a first PATCH appear to target a different file than the one the
		// resolver actually writes.
		if m.configDir == "" && m.configPath != "" {
			out = append(out, ConfigSource{Path: m.configPath, Writable: true})
		} else if m.managedPath != "" {
			// A config directory always has a managed destination, even when its
			// overlay has not been created yet.
			out = append(out, ConfigSource{Path: m.managedPath, Writable: true, Managed: true})
		}
	}
	return out, nil
}

func (m *ConfigManager) Snapshot(ctx context.Context) (ConfigSnapshot, error) {
	if m == nil {
		return ConfigSnapshot{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(ctx)
}

// snapshotLocked builds a source snapshot while m.mu is held.
func (m *ConfigManager) snapshotLocked(ctx context.Context) (ConfigSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	sources, err := m.sources()
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	data, err := mergedSourceYAML(sources)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	redacted, err := redactYAML(data)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	configJSON, err := redactedYAMLJSON(redacted)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	ownership, err := sourceOwnership(sources)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	etag, err := hashSources(sources)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	return ConfigSnapshot{Config: configJSON, YAML: string(redacted), ETag: etag, Sources: sources, Ownership: ownership, Writable: anyWritable(sources), RestartRequired: false}, nil
}

// ReadSources returns exact bytes for every configured source. It is a narrow
// seam for the settings transaction layer; callers must not mutate the map
// after the call and must use ApplySources for writes.
func (m *ConfigManager) ReadSources(ctx context.Context) (SourceFiles, error) {
	if m == nil {
		return SourceFiles{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SourceFiles{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sources, err := m.sources()
	if err != nil {
		return SourceFiles{}, err
	}
	data := make(map[string][]byte, len(sources))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return SourceFiles{}, err
		}
		content, readErr := os.ReadFile(source.Path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return SourceFiles{}, readErr
		}
		data[source.Path] = append([]byte(nil), content...)
	}
	etag, err := hashSources(sources)
	if err != nil {
		return SourceFiles{}, err
	}
	return SourceFiles{Sources: sources, Data: data, ETag: etag}, nil
}

// ApplySources atomically replaces one or more YAML source files. Every
// source is validated as the merged effective document before the first write;
// if disk or reload fails, all changed files are restored byte-for-byte.
func (m *ConfigManager) ApplySources(ctx context.Context, updates map[string][]byte, etag string) (ConfigSnapshot, error) {
	if m == nil {
		return ConfigSnapshot{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(updates) == 0 {
		return ConfigSnapshot{}, errors.New("source update cannot be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sources, err := m.sources()
	if err != nil {
		return ConfigSnapshot{}, err
	}
	currentETag, err := hashSources(sources)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if strings.TrimSpace(etag) == "" || etag != currentETag {
		return ConfigSnapshot{}, fmt.Errorf("etag conflict: current %s", currentETag)
	}
	activeConfig, activeErr := LoadConfigSources(m.configPath, m.configDir)
	if activeErr != nil {
		return ConfigSnapshot{}, activeErr
	}
	allowed := make(map[string]ConfigSource, len(sources))
	for _, source := range sources {
		allowed[source.Path] = source
	}
	for path, data := range updates {
		source, ok := allowed[path]
		if !ok {
			return ConfigSnapshot{}, fmt.Errorf("unknown config source %s", path)
		}
		if !source.Writable {
			return ConfigSnapshot{}, fmt.Errorf("config source %s is read-only", path)
		}
		if err := rejectSymlinkComponents(path); err != nil {
			return ConfigSnapshot{}, err
		}
		if current, readErr := os.ReadFile(path); readErr == nil {
			restored, restoreErr := restoreRedactedYAML(data, current)
			if restoreErr != nil {
				// Restoration is best-effort for documents without secrets,
				// but a candidate that still carries redaction placeholders
				// must never reach disk: writing them would overwrite the
				// real values with the literal "[REDACTED]" string.
				if bytes.Contains(data, []byte("[REDACTED]")) {
					return ConfigSnapshot{}, fmt.Errorf("config %s: could not restore redacted values: %w", path, restoreErr)
				}
			} else {
				updates[path] = restored
				data = restored
			}
		}
		if len(bytes.TrimSpace(data)) != 0 {
			var sourceDoc yaml.Node
			if err := yaml.Unmarshal(data, &sourceDoc); err != nil {
				return ConfigSnapshot{}, fmt.Errorf("parse %s: %w", path, err)
			}
			root := documentRoot(&sourceDoc)
			if root == nil || root.Kind != yaml.MappingNode {
				return ConfigSnapshot{}, fmt.Errorf("config %s: top-level YAML must be a mapping", path)
			}
		}
	}
	candidateYAML, err := mergedSourceYAMLWithUpdates(sources, updates)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if _, err := LoadConfigFromReader(bytes.NewReader(candidateYAML)); err != nil {
		return ConfigSnapshot{}, err
	}
	previousFiles := make([]previousFile, 0, len(updates))
	for path := range updates {
		old, readErr := os.ReadFile(path)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return ConfigSnapshot{}, readErr
		}
		previousFiles = append(previousFiles, previousFile{path: path, data: old, exists: exists})
	}
	for path, data := range updates {
		if err := atomicWriteWithBackup(path, data); err != nil {
			m.rollbackPreviousFiles(previousFiles)
			return ConfigSnapshot{}, err
		}
	}
	// The files are committed: any later failure must only roll back if no
	// concurrent ApplySources batch committed after this one started.
	m.applyGen++
	startGen := m.applyGen

	rollbackIfCurrent := func() {
		if m.applyGen != startGen {
			log.Printf("config: skipping rollback of %d file(s): a concurrent config apply committed meanwhile", len(previousFiles))
			return
		}
		m.rollbackPreviousFiles(previousFiles)
	}
	if err := ctx.Err(); err != nil {
		rollbackIfCurrent()
		return ConfigSnapshot{}, err
	}
	candidate, err := LoadConfigSources(m.configPath, m.configDir)
	if err != nil {
		rollbackIfCurrent()
		return ConfigSnapshot{}, err
	}
	fn := m.reload
	m.mu.Unlock()
	if fn != nil {
		err = fn(candidate)
	}
	m.mu.Lock()
	if err != nil {
		rollbackIfCurrent()
		return ConfigSnapshot{}, err
	}
	result, err := m.snapshotLocked(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	changes := Compare(activeConfig, candidate)
	result.RestartRequired = len(changes.RestartPaths) > 0
	result.RestartPaths = append([]string(nil), changes.RestartPaths...)
	return result, nil
}

// previousFile captures the pre-apply content of one written config source so
// a failed apply can restore it.
type previousFile struct {
	path   string
	data   []byte
	exists bool
}

// rollbackPreviousFiles restores the pre-apply contents of every written
// config source. Restore failures are logged, never swallowed: a file left
// half-rolled-back is the worst outcome a config apply can produce and must
// be visible to the operator.
func (m *ConfigManager) rollbackPreviousFiles(previousFiles []previousFile) {
	for _, old := range previousFiles {
		if err := restoreBytesIfExists(old.path, old.data, old.exists); err != nil {
			log.Printf("config: rollback of %s failed: %v (file left in applied state)", old.path, err)
		}
	}
}

// ValidateSourceUpdates validates a complete set of source replacements
// without writing them. It accepts partial source documents and validates only
// the merged effective configuration, which is required for layered YAML.
func (m *ConfigManager) ValidateSourceUpdates(ctx context.Context, updates map[string][]byte) (YAMLValidation, error) {
	if m == nil {
		return YAMLValidation{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return YAMLValidation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sources, err := m.sources()
	if err != nil {
		return YAMLValidation{}, err
	}
	allowed := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		allowed[source.Path] = struct{}{}
	}
	for path, data := range updates {
		if _, ok := allowed[path]; !ok {
			return YAMLValidation{Valid: false, Issues: []PatchIssue{{Path: path, Message: "unknown config source"}}}, nil
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return YAMLValidation{Valid: false, Issues: []PatchIssue{{Path: path, Message: err.Error()}}}, nil
		}
		if root := documentRoot(&doc); root == nil || root.Kind != yaml.MappingNode {
			return YAMLValidation{Valid: false, Issues: []PatchIssue{{Path: path, Message: "top-level YAML must be a mapping"}}}, nil
		}
	}
	merged, err := mergedSourceYAMLWithUpdates(sources, updates)
	if err != nil {
		return YAMLValidation{Valid: false, Issues: []PatchIssue{{Message: err.Error()}}}, nil
	}
	if _, err := LoadConfigFromReader(bytes.NewReader(merged)); err != nil {
		return YAMLValidation{Valid: false, Issues: []PatchIssue{{Path: validationPathFromError(err), Message: err.Error()}}}, nil
	}
	return YAMLValidation{Valid: true}, nil
}

func mergedSourceYAMLWithUpdates(sources []ConfigSource, updates map[string][]byte) ([]byte, error) {
	var merged *yaml.Node
	for _, source := range sources {
		data, ok := updates[source.Path]
		if !ok {
			var err error
			data, err = os.ReadFile(source.Path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", source.Path, err)
		}
		root := documentRoot(&doc)
		if root == nil || root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("config %s: top-level YAML must be a mapping", source.Path)
		}
		if merged == nil {
			merged = root
			continue
		}
		if err := mergeNodes(merged, root, "", source.Path); err != nil {
			return nil, err
		}
	}
	if merged == nil {
		merged = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	return yaml.Marshal(merged)
}

// sourceOwnership returns JSON-pointer ownership for the effective document.
// The map is intentionally informational: edits still go through
// resolvePatchTarget, which validates that all operations target one writable
// source. Later sources win when a scalar is repeated with the same value, just
// as the merge pipeline does.
func sourceOwnership(sources []ConfigSource) (map[string]string, error) {
	ownership := make(map[string]string)
	for _, source := range sources {
		data, err := os.ReadFile(source.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", source.Path, err)
		}
		root := &doc
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		walkOwnership(root, "", source.Path, ownership)
	}
	return ownership, nil
}

func walkOwnership(node *yaml.Node, path, source string, ownership map[string]string) {
	if node == nil {
		return
	}
	if path != "" {
		ownership[path] = source
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			walkOwnership(node.Content[i+1], path+"/"+escapeJSONPointer(key), source, ownership)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			walkOwnership(child, path+"/"+strconv.Itoa(i), source, ownership)
		}
	}
}

func escapeJSONPointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func hashSources(sources []ConfigSource) (string, error) {
	h := sha256.New()
	for _, source := range sources {
		info, err := os.Lstat(source.Path)
		if err != nil {
			// The managed overlay is advertised before its first write. Its
			// absence is part of the stable empty configuration state; once it
			// is created the path and bytes participate in the next ETag.
			if errors.Is(err, os.ErrNotExist) {
				h.Write([]byte(source.Path))
				h.Write([]byte{0})
				continue
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("config source %s must be a regular file", source.Path)
		}
		data, err := os.ReadFile(source.Path)
		if err != nil {
			return "", err
		}
		h.Write([]byte(source.Path))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`, nil
}

func anyWritable(sources []ConfigSource) bool {
	for _, source := range sources {
		if source.Writable {
			return true
		}
	}
	return false
}

func mergedSourceYAML(sources []ConfigSource) ([]byte, error) {
	var merged *yaml.Node
	for _, source := range sources {
		data, err := os.ReadFile(source.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var doc yaml.Node
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", source.Path, err)
		}
		root := &doc
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("config %s: top-level YAML must be a mapping", source.Path)
		}
		if merged == nil {
			merged = root
			continue
		}
		if err := mergeNodes(merged, root, "", source.Path); err != nil {
			return nil, err
		}
	}
	if merged == nil {
		merged = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	return yaml.Marshal(merged)
}

func redactYAML(data []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	redactNode(&node, "")
	return yaml.Marshal(&node)
}

// RedactYAMLForSettings exposes the same secret-safe renderer used by
// ConfigManager snapshots to the settings transaction layer.
func RedactYAMLForSettings(data []byte) ([]byte, error) { return redactYAML(data) }

// redactedYAMLJSON exposes the same source-aware, already-redacted document
// in a machine-readable form for control-plane clients. YAML decoding into
// interface{} can produce map[interface{}]interface{} for non-string keys;
// normalize those maps before JSON encoding so an unusual but valid metadata
// value cannot make GET /api/config fail after the YAML snapshot succeeded.
func redactedYAMLJSON(data []byte) (json.RawMessage, error) {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	normalized := normalizeYAMLJSONValue(value)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func normalizeYAMLJSONValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(current))
		for key, child := range current {
			out[key] = normalizeYAMLJSONValue(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(current))
		for key, child := range current {
			stringKey, ok := key.(string)
			if !ok {
				stringKey = fmt.Sprint(key)
			}
			out[stringKey] = normalizeYAMLJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(current))
		for index, child := range current {
			out[index] = normalizeYAMLJSONValue(child)
		}
		return out
	default:
		return value
	}
}

func redactNode(node *yaml.Node, parent string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			redactNode(child, parent)
		}
		return
	}
	// Model/backend env entries are already resolved when a Config is built,
	// but source YAML may also contain a literal value (for example
	// OPENAI_API_KEY=secret). Preserve a standalone ${env.NAME} placeholder so
	// the source contract remains visible while redacting every concrete value.
	if strings.EqualFold(parent, "env") && node.Kind == yaml.ScalarNode {
		redactEnvNode(node)
		return
	}
	// apiKeys accepts both legacy scalars and structured entries. Treat every
	// scalar below that collection as secret material, and redact key/value/
	// secret fields in its object form without touching unrelated YAML keys
	// named "key" elsewhere in the configuration.
	if strings.EqualFold(parent, "apikeys") && node.Kind == yaml.ScalarNode {
		node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", "[REDACTED]"
		node.Content = nil
		return
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			lower := strings.ToLower(key.Value)
			apiKeyField := strings.EqualFold(parent, "apikeys") && (lower == "key" || lower == "value" || lower == "secret")
			if apiKeyField || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token") || lower == "apikey" || lower == "api_key" {
				value.Kind, value.Tag, value.Value = yaml.ScalarNode, "!!str", "[REDACTED]"
				value.Content = nil
				continue
			}
			redactNode(value, key.Value)
		}
	} else if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			redactNode(child, parent)
		}
	}
}

func redactEnvNode(node *yaml.Node) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return
	}
	value := node.Value
	if envMacroRegex.MatchString(value) && envMacroRegex.FindString(value) == value {
		return
	}
	if index := strings.IndexByte(value, '='); index > 0 {
		if !IsSensitiveEnvironmentName(value[:index]) {
			return
		}
		right := value[index+1:]
		if envMacroRegex.MatchString(right) && envMacroRegex.FindString(right) == right {
			return
		}
		value = value[:index] + "=[REDACTED]"
	} else if IsSafeAnonymousEnvironmentValue(value) {
		// YAML may coerce an unquoted numeric environment item into a bare
		// scalar. These values are commonly GPU ordinals (for example 2, 3,
		// 4) and contain no credential material, so keep them visible.
		return
	} else {
		value = "[REDACTED]"
	}
	node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", value
	node.Content = nil
}

// IsSensitiveEnvironmentName reports whether an environment-variable name
// conventionally carries credentials. Runtime tuning, GPU placement and cache
// paths are operational configuration rather than secrets, so their values
// stay visible in the model editor while actual credentials remain redacted.
func IsSensitiveEnvironmentName(name string) bool {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
	if key == "key" || key == "apikey" || key == "api_key" || key == "authorization" || key == "password" || key == "secret" || key == "token" || key == "credential" {
		return true
	}
	return strings.Contains(key, "secret") || strings.Contains(key, "password") ||
		strings.Contains(key, "token") || strings.Contains(key, "credential") ||
		strings.Contains(key, "api_key") || strings.HasSuffix(key, "_key")
}

// IsSafeAnonymousEnvironmentValue reports whether a bare environment entry
// contains only decimal GPU ordinals, optionally separated by commas. Bare
// identifiers and other values remain redacted because their purpose cannot
// be established safely from the entry alone.
func IsSafeAnonymousEnvironmentValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, item := range strings.Split(value, ",") {
		if item == "" {
			return false
		}
		for _, char := range item {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func (m *ConfigManager) ValidatePatch(ctx context.Context, patchJSON []byte) (PatchValidation, error) {
	if m == nil {
		return PatchValidation{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PatchValidation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validatePatchLocked(ctx, patchJSON)
}

// ValidateYAML parses and semantically validates a complete configuration
// document. Redacted values from the current source document are restored
// before validation so a user can save the YAML returned by Snapshot without
// accidentally replacing secrets with the redaction marker.
func (m *ConfigManager) ValidateYAML(ctx context.Context, data []byte) (YAMLValidation, error) {
	if m == nil {
		return YAMLValidation{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return YAMLValidation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	validation, _, err := m.validateYAMLLocked(ctx, data)
	return validation, err
}

func (m *ConfigManager) validateYAMLLocked(ctx context.Context, data []byte) (YAMLValidation, []byte, error) {
	sources, err := m.sources()
	if err != nil {
		return YAMLValidation{}, nil, err
	}
	current, err := mergedSourceYAML(sources)
	if err != nil {
		return YAMLValidation{}, nil, err
	}
	canonical, err := restoreRedactedYAML(data, current)
	if err != nil {
		return YAMLValidation{Valid: false, Issues: []PatchIssue{{Message: err.Error()}}}, nil, nil
	}
	if _, err := LoadConfigFromReader(bytes.NewReader(canonical)); err != nil {
		return YAMLValidation{Valid: false, Issues: []PatchIssue{{Path: validationPathFromError(err), Message: err.Error()}}}, canonical, nil
	}
	if err := ctx.Err(); err != nil {
		return YAMLValidation{}, nil, err
	}
	return YAMLValidation{Valid: true}, canonical, nil
}

// ApplyYAML validates and atomically applies a complete YAML document. The
// actual write is delegated to ApplyPatch so source ownership, backups,
// reload rejection, and rollback all retain one implementation.
func (m *ConfigManager) ApplyYAML(ctx context.Context, data []byte, etag string) (ConfigSnapshot, YAMLValidation, error) {
	if m == nil {
		return ConfigSnapshot{}, YAMLValidation{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, YAMLValidation{}, err
	}
	m.mu.Lock()
	sources, err := m.sources()
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, YAMLValidation{}, err
	}
	currentETag, err := hashSources(sources)
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, YAMLValidation{}, err
	}
	if strings.TrimSpace(etag) == "" || etag != currentETag {
		m.mu.Unlock()
		return ConfigSnapshot{}, YAMLValidation{}, fmt.Errorf("etag conflict: current %s", currentETag)
	}
	validation, canonical, err := m.validateYAMLLocked(ctx, data)
	m.mu.Unlock()
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	if !validation.Valid {
		if len(validation.Issues) > 0 {
			return ConfigSnapshot{}, validation, fmt.Errorf("invalid YAML: %s", validation.Issues[0].Message)
		}
		return ConfigSnapshot{}, validation, errors.New("invalid YAML")
	}

	var value any
	if err := yaml.Unmarshal(canonical, &value); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	value = normalizeYAMLJSONValue(value)
	patchJSON, err := json.Marshal([]PatchOp{{Op: "replace", Path: "", Value: value}})
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	snapshot, patchValidation, err := m.ApplyPatch(ctx, patchJSON, etag)
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	if !patchValidation.Valid {
		return ConfigSnapshot{}, validation, errors.New("invalid YAML")
	}
	return snapshot, validation, nil
}

func restoreRedactedYAML(candidate, current []byte) ([]byte, error) {
	var candidateDoc, currentDoc yaml.Node
	if err := yaml.Unmarshal(candidate, &candidateDoc); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(current, &currentDoc); err != nil {
		return nil, err
	}
	candidateRoot := documentRoot(&candidateDoc)
	currentRoot := documentRoot(&currentDoc)
	if candidateRoot == nil {
		candidateRoot = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setDocumentRoot(&candidateDoc, candidateRoot)
	}
	if candidateRoot.Kind != yaml.MappingNode {
		return nil, errors.New("top-level YAML must be a mapping")
	}
	if currentRoot != nil {
		restoreRedactedNode(candidateRoot, currentRoot, "")
	}
	return yaml.Marshal(&candidateDoc)
}

func restoreRedactedNode(candidate, current *yaml.Node, parent string) {
	if candidate == nil || current == nil {
		return
	}
	if strings.EqualFold(parent, "env") && candidate.Kind == yaml.ScalarNode {
		if strings.Contains(candidate.Value, "[REDACTED]") {
			*candidate = *cloneYAMLNode(current)
		}
		return
	}
	if strings.EqualFold(parent, "apikeys") && candidate.Kind == yaml.ScalarNode {
		if candidate.Value == "[REDACTED]" {
			*candidate = *cloneYAMLNode(current)
		}
		return
	}
	switch candidate.Kind {
	case yaml.MappingNode:
		if current.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(candidate.Content); i += 2 {
			key, value := candidate.Content[i], candidate.Content[i+1]
			currentValue, found := mappingValue(current, key.Value)
			if !found {
				continue
			}
			lower := strings.ToLower(key.Value)
			sensitive := strings.EqualFold(parent, "apikeys") && (lower == "key" || lower == "value" || lower == "secret")
			sensitive = sensitive || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token") || lower == "apikey" || lower == "api_key"
			if sensitive && value.Kind == yaml.ScalarNode && value.Value == "[REDACTED]" {
				candidate.Content[i+1] = cloneYAMLNode(currentValue)
				continue
			}
			restoreRedactedNode(value, currentValue, key.Value)
		}
	case yaml.SequenceNode:
		if current.Kind != yaml.SequenceNode {
			return
		}
		for i, child := range candidate.Content {
			if i < len(current.Content) {
				restoreRedactedNode(child, current.Content[i], parent)
			}
		}
	}
}

func (m *ConfigManager) validatePatchLocked(ctx context.Context, patchJSON []byte) (PatchValidation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PatchValidation{}, err
	}
	ops, err := decodePatch(patchJSON)
	if err != nil {
		return PatchValidation{}, err
	}
	sources, err := m.sources()
	if err != nil {
		return PatchValidation{}, err
	}
	data, err := mergedSourceYAML(sources)
	if err != nil {
		return PatchValidation{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return PatchValidation{}, err
	}
	beforeKeys := ModelKeys(&doc)
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return PatchValidation{}, err
		}
		if err := applyPatchNode(&doc, op); err != nil {
			return PatchValidation{Valid: false, Issues: []PatchIssue{{Path: op.Path, Message: err.Error()}}, Diff: ops, ApplyMode: "atomic"}, nil
		}
	}
	// A patch that removes models leaves the routing blocks that named them
	// dangling, and the loader rejects those references outright — which would
	// make the delete that removed the model fail. Pruning the dangling
	// references makes the cascade part of the patch's own effect, so every
	// client (the UI, the delete endpoint, a hand-written patch) gets the same
	// result.
	pruned := PruneRemovedModels(&doc, RemovedModelKeys(beforeKeys, &doc))
	patched, err := yaml.Marshal(&doc)
	if err != nil {
		return PatchValidation{}, err
	}
	if _, err := LoadConfigFromReader(bytes.NewReader(patched)); err != nil {
		return PatchValidation{Valid: false, Issues: []PatchIssue{{Path: validationPathFromError(err), Message: err.Error()}}, Diff: ops, ApplyMode: "atomic"}, nil
	}
	if _, err := m.resolvePatchTarget(sources, ops); err != nil {
		return PatchValidation{Valid: false, Issues: []PatchIssue{{Path: "", Message: err.Error()}}, Diff: ops, ApplyMode: "atomic"}, nil
	}
	restartPaths := patchRestartPaths(ops)
	return PatchValidation{Valid: true, Diff: ops, ApplyMode: "atomic", RestartRequired: len(restartPaths) > 0, RestartPaths: restartPaths, Pruned: pruned}, nil
}

func decodePatch(data []byte) ([]PatchOp, error) {
	var rawOps []json.RawMessage
	if err := json.Unmarshal(data, &rawOps); err != nil {
		return nil, fmt.Errorf("RFC 6902 patch must be an array: %w", err)
	}
	if len(rawOps) == 0 {
		return nil, errors.New("patch cannot be empty")
	}
	ops := make([]PatchOp, 0, len(rawOps))
	for i, raw := range rawOps {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("RFC 6902 operation %d must be an object", i)
		}
		var opName string
		if value, ok := object["op"]; !ok || json.Unmarshal(value, &opName) != nil || opName == "" {
			return nil, fmt.Errorf("RFC 6902 operation %d requires a string op", i)
		}
		var path string
		if value, ok := object["path"]; !ok || json.Unmarshal(value, &path) != nil {
			return nil, fmt.Errorf("RFC 6902 operation %d requires a string path", i)
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("operation %d path must be a JSON pointer", i)
		}
		op := PatchOp{Op: opName, Path: path}
		if value, ok := object["value"]; ok {
			op.valueSet = true
			if err := json.Unmarshal(value, &op.Value); err != nil {
				return nil, fmt.Errorf("operation %d value: %w", i, err)
			}
		}
		if value, ok := object["from"]; ok {
			op.fromSet = true
			if err := json.Unmarshal(value, &op.From); err != nil {
				return nil, fmt.Errorf("operation %d from must be a string", i)
			}
		}
		switch op.Op {
		case "add", "replace", "test":
			if !op.valueSet {
				return nil, fmt.Errorf("operation %d (%s) requires value", i, op.Op)
			}
		case "remove":
			if op.valueSet {
				return nil, fmt.Errorf("operation %d (remove) must not include value", i)
			}
		case "copy", "move":
			if !op.fromSet {
				return nil, fmt.Errorf("operation %d (%s) requires from", i, op.Op)
			}
			if op.From != "" && !strings.HasPrefix(op.From, "/") {
				return nil, fmt.Errorf("operation %d from must be a JSON pointer", i)
			}
		default:
			return nil, fmt.Errorf("unsupported patch operation %q", op.Op)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func patchNeedsRestart(ops []PatchOp) bool {
	return len(patchRestartPaths(ops)) > 0
}

func patchRestartPaths(ops []PatchOp) []string {
	seen := make(map[string]struct{}, 3)
	paths := make([]string, 0, 3)
	add := func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for _, op := range ops {
		for _, path := range []string{"/store/path", "/runtimeManager/root", "/logToStdout"} {
			if jsonPointerUnder(op.Path, path) {
				add(path)
			}
			if (op.Op == "copy" || op.Op == "move") && jsonPointerUnder(op.From, path) {
				add(path)
			}
		}
	}
	return paths
}

func jsonPointerUnder(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func pointerParts(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if path[0] != '/' {
		return nil, errors.New("invalid JSON pointer")
	}
	parts := strings.Split(path[1:], "/")
	for i, p := range parts {
		var decoded strings.Builder
		for j := 0; j < len(p); j++ {
			if p[j] != '~' {
				decoded.WriteByte(p[j])
				continue
			}
			if j+1 >= len(p) || (p[j+1] != '0' && p[j+1] != '1') {
				return nil, fmt.Errorf("invalid JSON pointer escape in %q", path)
			}
			if p[j+1] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
			j++
		}
		parts[i] = decoded.String()
	}
	return parts, nil
}

func applyPatchNode(doc *yaml.Node, op PatchOp) error {
	parts, err := pointerParts(op.Path)
	if err != nil {
		return err
	}
	root := documentRoot(doc)
	if root == nil {
		return errors.New("YAML document has no root")
	}
	if op.Op == "copy" || op.Op == "move" {
		fromParts, fromErr := pointerParts(op.From)
		if fromErr != nil {
			return fromErr
		}
		if op.Op == "move" && pointerIsDescendant(fromParts, parts) {
			return fmt.Errorf("move path %q cannot contain from path %q", op.Path, op.From)
		}
		source, found := nodeAt(root, fromParts)
		if !found {
			return fmt.Errorf("from path %q does not exist", op.From)
		}
		value := cloneYAMLNode(source)
		if op.Op == "move" {
			backup := cloneYAMLNode(doc)
			if err := removeAt(doc, root, fromParts); err != nil {
				return err
			}
			if err := addAt(doc, documentRoot(doc), parts, value); err != nil {
				*doc = *backup
				return err
			}
			return nil
		}
		return addAt(doc, root, parts, value)
	}
	if op.Op == "test" {
		value := &yaml.Node{}
		if err := value.Encode(op.Value); err != nil {
			return err
		}
		actual, found := nodeAt(root, parts)
		if !found || !yamlNodesEqual(actual, value) {
			return fmt.Errorf("test operation failed at path %q", op.Path)
		}
		return nil
	}
	if len(parts) == 0 {
		if op.Op == "remove" {
			return errors.New("removing the YAML document root is not supported")
		}
		value := &yaml.Node{}
		if err := value.Encode(op.Value); err != nil {
			return err
		}
		setDocumentRoot(doc, value)
		return nil
	}
	value := &yaml.Node{}
	if op.Op != "remove" {
		if err := value.Encode(op.Value); err != nil {
			return err
		}
	}
	switch op.Op {
	case "add":
		return addAt(doc, root, parts, value)
	case "replace":
		return replaceAt(root, parts, value)
	case "remove":
		return removeAt(doc, root, parts)
	default:
		return fmt.Errorf("unsupported patch operation %q", op.Op)
	}
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func setDocumentRoot(doc, value *yaml.Node) {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			doc.Content = []*yaml.Node{value}
		} else {
			preserveYAMLNodeMetadata(doc.Content[0], value)
			doc.Content[0] = value
		}
		return
	}
	preserveYAMLNodeMetadata(doc, value)
	*doc = *value
}

func nodeAt(root *yaml.Node, parts []string) (*yaml.Node, bool) {
	current := root
	for _, part := range parts {
		child, ok := nodeChild(current, part)
		if !ok {
			return nil, false
		}
		current = child
	}
	return current, current != nil
}

func addAt(doc, root *yaml.Node, parts []string, value *yaml.Node) error {
	if len(parts) == 0 {
		setDocumentRoot(doc, value)
		return nil
	}
	parent := root
	for _, part := range parts[:len(parts)-1] {
		child, ok := nodeChild(parent, part)
		if !ok {
			// RFC 6902 permits creating the final member of an existing
			// container, but never creates missing intermediate parents. Keeping
			// this strict prevents a typo such as /models/new/backend from
			// silently manufacturing a second configuration tree.
			return fmt.Errorf("path %q parent does not exist", "/"+strings.Join(parts, "/"))
		}
		parent = child
	}
	key := parts[len(parts)-1]
	switch parent.Kind {
	case yaml.MappingNode:
		setMappingValue(parent, key, value)
		return nil
	case yaml.SequenceNode:
		idx, err := sequenceIndex(key, len(parent.Content), true)
		if err != nil {
			return fmt.Errorf("path %q: %w", "/"+strings.Join(parts, "/"), err)
		}
		parent.Content = append(parent.Content, nil)
		copy(parent.Content[idx+1:], parent.Content[idx:])
		parent.Content[idx] = value
		return nil
	default:
		return fmt.Errorf("path %q parent is not a mapping or sequence", "/"+strings.Join(parts, "/"))
	}
}

func replaceAt(root *yaml.Node, parts []string, value *yaml.Node) error {
	if len(parts) == 0 {
		return errors.New("replaceAt requires a non-root path")
	}
	parent, key, err := patchParent(root, parts)
	if err != nil {
		return err
	}
	switch parent.Kind {
	case yaml.MappingNode:
		// A settings form targets the parsed config, which carries loader
		// defaults even for keys the source YAML never declared. Replacing
		// such a key must create it rather than error, so the value the user
		// just edited lands in the file. setMappingValue already replaces an
		// existing member and appends a missing one.
		setMappingValue(parent, key, value)
	case yaml.SequenceNode:
		idx, err := sequenceIndex(key, len(parent.Content), false)
		if err != nil || idx >= len(parent.Content) {
			if err == nil {
				err = errors.New("path does not exist")
			}
			return fmt.Errorf("path %q: %w", "/"+strings.Join(parts, "/"), err)
		}
		preserveYAMLNodeMetadata(parent.Content[idx], value)
		parent.Content[idx] = value
	default:
		return fmt.Errorf("path %q parent is not a mapping or sequence", "/"+strings.Join(parts, "/"))
	}
	return nil
}

func removeAt(_ *yaml.Node, root *yaml.Node, parts []string) error {
	if len(parts) == 0 {
		return errors.New("removing the YAML document root is not supported")
	}
	parent, key, err := patchParent(root, parts)
	if err != nil {
		return err
	}
	switch parent.Kind {
	case yaml.MappingNode:
		idx := mappingIndex(parent, key)
		if idx < 0 {
			return fmt.Errorf("path %q does not exist", "/"+strings.Join(parts, "/"))
		}
		parent.Content = append(parent.Content[:idx], parent.Content[idx+2:]...)
	case yaml.SequenceNode:
		idx, err := sequenceIndex(key, len(parent.Content), false)
		if err != nil || idx >= len(parent.Content) {
			if err == nil {
				err = errors.New("path does not exist")
			}
			return fmt.Errorf("path %q: %w", "/"+strings.Join(parts, "/"), err)
		}
		parent.Content = append(parent.Content[:idx], parent.Content[idx+1:]...)
	default:
		return fmt.Errorf("path %q parent is not a mapping or sequence", "/"+strings.Join(parts, "/"))
	}
	return nil
}

func patchParent(root *yaml.Node, parts []string) (*yaml.Node, string, error) {
	parent, found := nodeAt(root, parts[:len(parts)-1])
	if !found {
		return nil, "", fmt.Errorf("path %q parent does not exist", "/"+strings.Join(parts, "/"))
	}
	return parent, parts[len(parts)-1], nil
}

func pointerIsDescendant(from, path []string) bool {
	if len(path) <= len(from) {
		return false
	}
	for i := range from {
		if from[i] != path[i] {
			return false
		}
	}
	return true
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	copyNode := *node
	copyNode.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copyNode.Content[i] = cloneYAMLNode(child)
	}
	return &copyNode
}

func yamlNodesEqual(left, right *yaml.Node) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value || len(left.Content) != len(right.Content) {
		return false
	}
	for i := range left.Content {
		if !yamlNodesEqual(left.Content[i], right.Content[i]) {
			return false
		}
	}
	return true
}

func nodeChild(node *yaml.Node, part string) (*yaml.Node, bool) {
	if node == nil {
		return nil, false
	}
	if node.Kind == yaml.MappingNode {
		return mappingValue(node, part)
	}
	if node.Kind == yaml.SequenceNode {
		idx, err := sequenceIndex(part, len(node.Content), false)
		if err != nil || idx >= len(node.Content) {
			return nil, false
		}
		return node.Content[idx], true
	}
	return nil, false
}

func sequenceIndex(token string, length int, allowAppend bool) (int, error) {
	if allowAppend && token == "-" {
		return length, nil
	}
	if token == "" || strings.HasPrefix(token, "+") || strings.HasPrefix(token, "-") {
		return 0, errors.New("sequence index must be a non-negative integer")
	}
	index, err := strconv.Atoi(token)
	if err != nil || index < 0 {
		return 0, errors.New("sequence index must be a non-negative integer")
	}
	if index > length || (!allowAppend && index == length) {
		return 0, errors.New("sequence index is out of range")
	}
	return index, nil
}

func mappingIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}
func mappingValue(node *yaml.Node, key string) (*yaml.Node, bool) {
	idx := mappingIndex(node, key)
	if idx < 0 {
		return nil, false
	}
	return node.Content[idx+1], true
}
func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	idx := mappingIndex(node, key)
	if idx >= 0 {
		preserveYAMLNodeMetadata(node.Content[idx+1], value)
		node.Content[idx+1] = value
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// preserveYAMLNodeMetadata carries comments and presentation metadata from an
// existing node onto a JSON-patch replacement. yaml.Node.Encode creates a
// fresh node for patch values, so replacing a mapping member or sequence item
// directly would otherwise erase a user's line comments even though the
// surrounding source file remains intact. Semantic value fields intentionally
// stay on the replacement node; only source-facing metadata is inherited.
func preserveYAMLNodeMetadata(from, to *yaml.Node) {
	if from == nil || to == nil {
		return
	}
	to.Style = from.Style
	to.Anchor = from.Anchor
	to.HeadComment = from.HeadComment
	to.LineComment = from.LineComment
	to.FootComment = from.FootComment
	to.Line = from.Line
	to.Column = from.Column
}

// seedPatchParents materializes only those mapping parents that are inherited
// from another YAML source.  ApplyPatch edits a single source file, so an
// overlay containing `models: {}` must receive that parent before adding a
// new model.  The effective document is authoritative for which parents are
// allowed to exist; arbitrary paths are never manufactured here.  RFC 6902
// validation still runs against the merged document and remains strict about
// missing intermediate parents.
func seedPatchParents(targetDoc, effectiveDoc *yaml.Node, op PatchOp) error {
	parts, err := pointerParts(op.Path)
	if err != nil {
		return err
	}
	if len(parts) < 2 {
		return nil
	}
	targetRoot := documentRoot(targetDoc)
	effectiveRoot := documentRoot(effectiveDoc)
	if targetRoot == nil || effectiveRoot == nil {
		return errors.New("YAML document has no root")
	}
	for depth := 1; depth < len(parts); depth++ {
		prefix := parts[:depth]
		if _, found := nodeAt(targetRoot, prefix); found {
			continue
		}
		effectiveParent, found := nodeAt(effectiveRoot, prefix)
		if !found {
			return fmt.Errorf("path %q parent does not exist", op.Path)
		}
		if effectiveParent.Kind != yaml.MappingNode {
			return fmt.Errorf("path %q inherited parent is not a mapping", op.Path)
		}
		if err := ensureMappingPath(targetRoot, prefix); err != nil {
			return fmt.Errorf("path %q: %w", op.Path, err)
		}
	}
	return nil
}

func ensureMappingPath(root *yaml.Node, parts []string) error {
	current := root
	for _, part := range parts {
		if current == nil || current.Kind != yaml.MappingNode {
			return errors.New("parent is not a mapping")
		}
		if child, found := nodeChild(current, part); found {
			current = child
			continue
		}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(current, part, child)
		current = child
	}
	return nil
}

// ApplyPatch atomically edits the source selected by the first entity path.
// Existing files are backed up beside themselves; new entities go to the
// managed source. The reload callback can reject the candidate and trigger a
// byte-for-byte rollback.
func (m *ConfigManager) ApplyPatch(ctx context.Context, patchJSON []byte, etag string) (ConfigSnapshot, PatchValidation, error) {
	if m == nil {
		return ConfigSnapshot{}, PatchValidation{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, PatchValidation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sources, err := m.sources()
	if err != nil {
		return ConfigSnapshot{}, PatchValidation{}, err
	}
	currentETag, err := hashSources(sources)
	if err != nil {
		return ConfigSnapshot{}, PatchValidation{}, err
	}
	if strings.TrimSpace(etag) == "" || etag != currentETag {
		return ConfigSnapshot{}, PatchValidation{}, fmt.Errorf("etag conflict: current %s", currentETag)
	}
	validation, err := m.validatePatchLocked(ctx, patchJSON)
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	if !validation.Valid {
		if len(validation.Issues) > 0 {
			return ConfigSnapshot{}, validation, fmt.Errorf("invalid config patch at %s: %s", validation.Issues[0].Path, validation.Issues[0].Message)
		}
		return ConfigSnapshot{}, validation, errors.New("invalid config patch")
	}
	ops, _ := decodePatch(patchJSON)
	target, err := m.resolvePatchTarget(sources, ops)
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	// Apply the patch to the source that defines the entity (or to the
	// managed overlay for a new entity). Editing the merged effective document
	// would duplicate every inherited key into one source and would destroy
	// source ownership/comments on the next reload.
	var doc yaml.Node
	existing, readErr := os.ReadFile(target)
	if readErr == nil && len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &doc); err != nil {
			return ConfigSnapshot{}, validation, err
		}
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return ConfigSnapshot{}, validation, readErr
	} else {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	// The target source may be an overlay that does not repeat parents defined
	// in an earlier source.  Seed only those inherited mapping parents so the
	// strict RFC 6902 application below can still edit the selected source.
	var effectiveDoc yaml.Node
	effectiveYAML, effectiveErr := mergedSourceYAML(sources)
	if effectiveErr != nil {
		return ConfigSnapshot{}, validation, effectiveErr
	}
	if err := yaml.Unmarshal(effectiveYAML, &effectiveDoc); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	// The pre-patch model set, read from the same merged bytes, so the pruning
	// below can tell which models the patch actually removed.
	var originalEffective yaml.Node
	if err := yaml.Unmarshal(effectiveYAML, &originalEffective); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return ConfigSnapshot{}, validation, err
		}
		if err := seedPatchParents(&doc, &effectiveDoc, op); err != nil {
			return ConfigSnapshot{}, validation, err
		}
		if err := applyPatchNode(&doc, op); err != nil {
			return ConfigSnapshot{}, validation, err
		}
		// Keep the effective working copy in lockstep.  This allows a later
		// operation in the same patch to target a parent introduced by an
		// earlier operation while preserving strict pointer semantics.
		if err := applyPatchNode(&effectiveDoc, op); err != nil {
			return ConfigSnapshot{}, validation, err
		}
	}
	// A removed model leaves the routing blocks that named it dangling, and the
	// loader rejects those references — which would roll the write back and fail
	// the delete. The merged view decides what is actually gone (a model removed
	// and re-added by another source is not gone), and the references are
	// pruned from the source being written. References held by a different
	// source are not rewritten here; the loader below catches them and the write
	// is rolled back, so a partially dangling configuration is never committed.
	validation.Pruned = PruneRemovedModels(&doc, RemovedModelKeys(ModelKeys(&originalEffective), &effectiveDoc))
	// Sensitive fields reach the editor as "[REDACTED]" placeholders. Restore
	// every sentinel the patch left untouched from the pre-patch effective
	// document (effectiveDoc was mutated by the same operations, so the
	// original bytes are decoded into a fresh node) so that saving an
	// unrelated field never overwrites a secret with its placeholder. Values
	// the editor actually changed do not match the sentinel and are written
	// as-is.
	var restoreDoc yaml.Node
	if err := yaml.Unmarshal(effectiveYAML, &restoreDoc); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	restoreRedactedNode(documentRoot(&doc), documentRoot(&restoreDoc), "")
	patched, marshalErr := yaml.Marshal(&doc)
	if marshalErr != nil {
		// The write below is the irreversible step of this transaction, so a
		// marshal failure has to stop here: patched would be nil and
		// atomicWriteWithBackup would persist a zero-byte config over the
		// source the operator is editing.
		return ConfigSnapshot{}, validation, fmt.Errorf("marshal patched config %s: %w", target, marshalErr)
	}
	old, oldReadErr := os.ReadFile(target)
	oldExists := oldReadErr == nil
	if oldReadErr != nil && !os.IsNotExist(oldReadErr) {
		return ConfigSnapshot{}, validation, oldReadErr
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	if err := atomicWriteWithBackup(target, patched); err != nil {
		return ConfigSnapshot{}, validation, err
	}
	// The patch file is committed; a later failure may only roll back if no
	// concurrent apply (ApplySources or another ApplyPatch) committed after
	// this point, or the rollback would discard that newer configuration.
	m.applyGen++
	patchGen := m.applyGen
	// The file write is the first irreversible step of this transaction. If a
	// request is canceled immediately afterwards, restore the exact previous
	// bytes before invoking the reload callback; otherwise a disconnected client
	// could leave a patch on disk that no running Server has accepted.
	if err := ctx.Err(); err != nil {
		restoreErr := restoreBytesIfExists(target, old, oldExists)
		return ConfigSnapshot{}, validation, errors.Join(err, restoreErr)
	}
	candidate, err := LoadConfigSources(m.configPath, m.configDir)
	if err != nil {
		if restoreErr := restoreBytesIfExists(target, old, oldExists); restoreErr != nil {
			log.Printf("config: rollback of %s failed: %v (file left in patched state)", target, restoreErr)
		}
		return ConfigSnapshot{}, validation, err
	}
	if err := ctx.Err(); err != nil {
		restoreErr := restoreBytesIfExists(target, old, oldExists)
		return ConfigSnapshot{}, validation, errors.Join(err, restoreErr)
	}
	// Capture the callback while holding the manager lock. Calling it after
	// unlocking avoids deadlocks (reload may synchronously inspect config),
	// while the captured function keeps SetReload race-free.
	fn := m.reload
	m.mu.Unlock()
	if fn != nil {
		err = fn(candidate)
	}
	m.mu.Lock()
	if err != nil {
		// A concurrent apply that committed while the reload callback ran owns
		// the newer file state: rolling back over it would discard their
		// committed configuration.
		if m.applyGen == patchGen {
			if restoreErr := restoreBytesIfExists(target, old, oldExists); restoreErr != nil {
				log.Printf("config: rollback of %s failed: %v (file left in patched state)", target, restoreErr)
			}
		} else {
			log.Printf("config: skipping rollback of %s: a concurrent config apply committed meanwhile", target)
		}
		return ConfigSnapshot{}, validation, err
	}
	// The patch is on disk now, so the snapshot must describe what was actually
	// written. Discarding these errors was worse than failing: an unreadable
	// source set left newSources empty, and the empty set hashed and marshalled
	// cleanly — the API then answered "applied" with a null config and an ETag
	// over nothing, which the next preview/commit compares against.
	newSources, sourcesErr := m.sources()
	if sourcesErr != nil {
		return ConfigSnapshot{}, validation, sourcesErr
	}
	snapData, mergeErr := mergedSourceYAML(newSources)
	if mergeErr != nil {
		return ConfigSnapshot{}, validation, mergeErr
	}
	redacted, redactErr := redactYAML(snapData)
	if redactErr != nil {
		return ConfigSnapshot{}, validation, redactErr
	}
	configJSON, configErr := redactedYAMLJSON(redacted)
	if configErr != nil {
		return ConfigSnapshot{}, validation, configErr
	}
	ownership, ownershipErr := sourceOwnership(newSources)
	if ownershipErr != nil {
		return ConfigSnapshot{}, validation, ownershipErr
	}
	etag, err = hashSources(newSources)
	if err != nil {
		return ConfigSnapshot{}, validation, err
	}
	return ConfigSnapshot{Config: configJSON, YAML: string(redacted), ETag: etag, Sources: newSources, Ownership: ownership, Writable: anyWritable(newSources), RestartRequired: validation.RestartRequired, RestartPaths: append([]string(nil), validation.RestartPaths...)}, validation, nil
}

// resolvePatchTarget picks the YAML source that owns every operation. Existing
// paths are written back to the source where they are defined; new entities in
// a config directory are written to the managed overlay. A patch spanning two
// source files is rejected instead of flattening both files into one.
func (m *ConfigManager) resolvePatchTarget(sources []ConfigSource, ops []PatchOp) (string, error) {
	var target string
	for _, op := range ops {
		parts, err := pointerParts(op.Path)
		if err != nil {
			return "", err
		}
		if op.Op == "copy" || op.Op == "move" {
			fromParts, fromErr := pointerParts(op.From)
			if fromErr != nil {
				return "", fromErr
			}
			fromOwner, fromExact, ownerErr := sourceOwner(sources, fromParts, "replace")
			if ownerErr != nil {
				return "", ownerErr
			}
			if !fromExact || fromOwner == "" {
				return "", fmt.Errorf("from path %q is not defined in a config source", op.From)
			}
			// A single ApplyPatch transaction edits one source file. Copying or
			// moving across files would either flatten inherited settings or
			// silently leave the source side unchanged, so reject it explicitly.
			if target != "" && target != fromOwner {
				return "", fmt.Errorf("patch spans multiple config sources (%s and %s)", target, fromOwner)
			}
			if target == "" {
				target = fromOwner
			}
		}
		ownerOp := op.Op
		if ownerOp == "copy" || ownerOp == "move" {
			ownerOp = "add"
		}
		owner, exact, err := sourceOwner(sources, parts, ownerOp)
		if err != nil && op.Op == "replace" {
			// The settings form edits the parsed config, which carries loader
			// defaults for keys the source YAML never declared. A replace of
			// such a key creates it, so ownership resolves like an add.
			owner, exact, err = sourceOwner(sources, parts, "add")
		}
		if err != nil {
			return "", err
		}
		if owner == "" || (m.configDir != "" && !exact && isNewIdentityEntity(sources, parts)) {
			// Identity-keyed additions (models, groups, peers, etc.) are new
			// entities even when their parent map exists, so they belong in the
			// managed overlay.
			if m.configDir != "" && isNewIdentityEntity(sources, parts) {
				owner = m.managedPath
			} else if m.configDir == "" && m.configPath != "" {
				owner = m.configPath
			} else {
				owner = m.managedPath
			}
		}
		if exact {
			for _, source := range sources {
				if source.Path == owner && !source.Writable {
					return "", fmt.Errorf("config source %s is read-only", owner)
				}
			}
		}
		if target == "" {
			target = owner
		} else if target != owner {
			return "", fmt.Errorf("patch spans multiple config sources (%s and %s)", target, owner)
		}
	}
	if target == "" {
		return "", errors.New("patch has no target")
	}
	return target, nil
}

// sourceOwner returns the source with the longest matching path. For add,
// matching the parent is sufficient; for replace/remove an exact path is
// required by validation and is reported through exact=false otherwise.
func sourceOwner(sources []ConfigSource, parts []string, op string) (owner string, exact bool, err error) {
	bestDepth := -1
	bestExact := false
	for _, source := range sources {
		data, readErr := os.ReadFile(source.Path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return "", false, readErr
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil {
			continue
		}
		root := &doc
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		depth := matchingPathDepth(root, parts)
		// Later sources are merged over earlier sources. When two files define
		// the same path at the same depth, the later source therefore owns the
		// effective value and is the only file that should be edited. An exact
		// path in an earlier source must not be shadowed by an unrelated later
		// source that merely shares a parent (both have the same depth).
		exactPath := depth == len(parts)
		if depth > bestDepth || (depth == bestDepth && exactPath && !bestExact) || (depth == bestDepth && exactPath == bestExact) {
			bestDepth = depth
			owner = source.Path
			exact = exactPath
			bestExact = exactPath
		}
	}
	if bestDepth < 0 {
		return "", false, nil
	}
	if op != "add" && !exact {
		return "", false, fmt.Errorf("path %q is not defined in a writable config source", "/"+strings.Join(parts, "/"))
	}
	return owner, exact, nil
}

func matchingPathDepth(root *yaml.Node, parts []string) int {
	current := root
	depth := 0
	for _, part := range parts {
		child, ok := nodeChild(current, part)
		if !ok {
			break
		}
		current = child
		depth++
	}
	return depth
}

func isNewIdentityEntity(sources []ConfigSource, parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	// Collections whose entries are addressed by a stable identity are safe to
	// extend in the managed overlay.  This is important for read-only base
	// files: adding a runtime must not rewrite the distribution-provided YAML.
	collectionPath := parts[:1]
	identityIndex := 1
	identity := map[string]bool{
		"models":    true,
		"groups":    true,
		"profiles":  true,
		"selectors": true,
		"peers":     true,
		"runtimes":  true,
	}
	if !identity[parts[0]] {
		// modelFiles.sources is the one nested map collection currently exposed
		// as a named source catalog. Keep the special case explicit rather than
		// treating every nested mapping as an identity (which could redirect a
		// scalar patch to the overlay unexpectedly).
		if len(parts) < 3 || parts[0] != "modelFiles" || parts[1] != "sources" {
			return false
		}
		collectionPath = parts[:2]
		identityIndex = 2
	}
	if len(parts) <= identityIndex {
		return false
	}
	foundIdentity := false
	for _, source := range sources {
		data, err := os.ReadFile(source.Path)
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil {
			continue
		}
		root := &doc
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		collection := root
		ok := true
		for _, part := range collectionPath {
			collection, ok = nodeChild(collection, part)
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		if _, exists := nodeChild(collection, parts[identityIndex]); exists {
			foundIdentity = true
		}
	}
	return !foundIdentity
}

func atomicWriteWithBackup(path string, data []byte) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("config source %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("config source %s must not be a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config source %s must be a regular file", path)
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".llama-swap-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameConfigFile(tmpName, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) {
			return err
		}
		// A file bind-mounted into a Docker container is itself a mount point,
		// so the kernel rejects replacing it with rename(2). Keep the normal
		// atomic rename for regular files and use a durable in-place write only
		// for this mount-specific case; replacing the contents is the only way
		// to update a single-file bind mount without asking users to remount a
		// configuration directory.
		if writeErr := writeConfigInPlace(path, data, mode); writeErr != nil {
			return errors.Join(err, writeErr)
		}
		return nil
	}
	return syncDirectory(filepath.Dir(path))
}

var renameConfigFile = os.Rename

func writeConfigInPlace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err != nil {
		_ = f.Close()
		return err
	}
	if n != len(data) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func restoreBytes(path string, data []byte) error {
	return restoreBytesIfExists(path, data, true)
}

func restoreBytesIfExists(path string, data []byte, exists bool) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("config source %s: %w", path, err)
	}
	if !exists {
		return os.Remove(path)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("config source %s must not be a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config source %s must be a regular file", path)
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Rollback restores the last source backup and validates the resulting config.
func (m *ConfigManager) Rollback(ctx context.Context) (ConfigSnapshot, error) {
	if m == nil {
		return ConfigSnapshot{}, errors.New("config manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConfigSnapshot{}, err
	}
	m.mu.Lock()
	sources, err := m.sources()
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	// One ApplyPatch transaction updates one source, but several sources may
	// have backups from earlier edits. Pick the newest backup rather than the
	// first source in merge order; otherwise rollback could silently restore an
	// unrelated, older file. Include absent config/managed files whose backup
	// remains after a failed or interrupted write.
	paths := make([]string, 0, len(sources)+2)
	seen := make(map[string]struct{}, len(sources)+2)
	addPath := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for _, source := range sources {
		addPath(source.Path)
	}
	addPath(m.configPath)
	addPath(m.managedPath)
	var backupPath string
	var backupTime time.Time
	for _, path := range paths {
		backup := path + ".bak"
		info, statErr := os.Lstat(backup)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			m.mu.Unlock()
			return ConfigSnapshot{}, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			m.mu.Unlock()
			return ConfigSnapshot{}, fmt.Errorf("config backup %s must not be a symlink", backup)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if backupPath == "" || info.ModTime().After(backupTime) || (info.ModTime().Equal(backupTime) && backup < backupPath) {
			backupPath, backupTime = backup, info.ModTime()
		}
	}
	if backupPath != "" {
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return ConfigSnapshot{}, err
		}
		target := strings.TrimSuffix(backupPath, ".bak")
		if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			m.mu.Unlock()
			return ConfigSnapshot{}, fmt.Errorf("config source %s must not be a symlink", target)
		}
		backup, readErr := os.ReadFile(backupPath)
		if readErr != nil {
			m.mu.Unlock()
			return ConfigSnapshot{}, readErr
		}
		if err := restoreBytes(target, backup); err != nil {
			m.mu.Unlock()
			return ConfigSnapshot{}, err
		}
	}
	if _, err := LoadConfigSources(m.configPath, m.configDir); err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	newSources, _ := m.sources()
	data, err := mergedSourceYAML(newSources)
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	redacted, err := redactYAML(data)
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	configJSON, err := redactedYAMLJSON(redacted)
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	ownership, ownershipErr := sourceOwnership(newSources)
	if ownershipErr != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, ownershipErr
	}
	etag, err := hashSources(newSources)
	if err != nil {
		m.mu.Unlock()
		return ConfigSnapshot{}, err
	}
	snap := ConfigSnapshot{Config: configJSON, YAML: string(redacted), ETag: etag, Sources: newSources, Ownership: ownership, Writable: anyWritable(newSources)}
	fn := m.reload
	m.mu.Unlock()
	if fn != nil {
		candidate, loadErr := LoadConfigSources(m.configPath, m.configDir)
		if loadErr != nil {
			return ConfigSnapshot{}, loadErr
		}
		if err := fn(candidate); err != nil {
			return ConfigSnapshot{}, err
		}
	}
	return snap, nil
}
