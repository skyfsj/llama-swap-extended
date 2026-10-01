// Package settings owns the control-plane configuration Interface. It keeps
// the YAML source manager behind a small Draft/Preview/Commit seam so HTTP and
// UI callers never need to know about RFC 6902 or source ownership details.
package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/settings/spec"
)

const (
	ModeStructured = "structured"
	ModeSources    = "sources"
	ModeRestore    = "restore"
	historyMinimum = 100
	historyMaxAge  = 90 * 24 * time.Hour
	historySoftCap = 256 << 20
)

type Actor struct {
	ID     string `json:"id"`
	Origin string `json:"origin"`
}

type Change struct {
	Op       string `json:"op"`
	Path     string `json:"path"`
	Value    any    `json:"value,omitempty"`
	SourceID string `json:"sourceId,omitempty"`
}

type SourceDraft struct {
	SourceID string `json:"sourceId"`
	YAML     string `json:"yaml"`
}

type Draft struct {
	Mode            string        `json:"mode"`
	Changes         []Change      `json:"changes,omitempty"`
	Sources         []SourceDraft `json:"sources,omitempty"`
	RestoreRevision string        `json:"restoreRevision,omitempty"`
	Message         string        `json:"message,omitempty"`
}

type Diagnostic struct {
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	SourceID string `json:"sourceId,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Impact struct {
	Class string   `json:"class"` // hot, model-restart, daemon-restart
	Paths []string `json:"paths"`
}

type Preview struct {
	Valid        bool         `json:"valid"`
	BaseETag     string       `json:"baseEtag"`
	PreviewHash  string       `json:"previewHash"`
	Diff         []Change     `json:"diff,omitempty"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
	Impacts      []Impact     `json:"impacts,omitempty"`
	Migration    []string     `json:"migration,omitempty"`
	SourceTarget []string     `json:"sourceTargets,omitempty"`
}

type Snapshot struct {
	Revision       string                `json:"revision"`
	ETag           string                `json:"etag"`
	Config         json.RawMessage       `json:"config,omitempty"`
	YAML           string                `json:"yaml"`
	Sources        []config.ConfigSource `json:"sources"`
	SourceYAML     map[string]string     `json:"sourceYaml,omitempty"`
	Ownership      map[string]string     `json:"ownership,omitempty"`
	Writable       bool                  `json:"writable"`
	Warnings       []Diagnostic          `json:"warnings,omitempty"`
	PendingRestart bool                  `json:"pendingRestart"`
	RestartPaths   []string              `json:"restartPaths,omitempty"`
	SchemaVersion  int                   `json:"schemaVersion"`
}

type CommitResult struct {
	Snapshot Snapshot `json:"snapshot"`
	Revision Revision `json:"revision"`
	Preview  Preview  `json:"preview"`
}

type Revision struct {
	ID        string    `json:"id"`
	Parent    string    `json:"parent,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Actor     Actor     `json:"actor"`
	Message   string    `json:"message,omitempty"`
	Origin    string    `json:"origin"`
	Result    string    `json:"result"`
	Changed   []string  `json:"changed,omitempty"`
	Impacts   []Impact  `json:"impacts,omitempty"`
	// Sources maps the original absolute source path to its private snapshot
	// filename. The files are never returned by HTTP APIs.
	Sources     map[string]string `json:"sources,omitempty"`
	SnapshotDir string            `json:"-"`
}

type Manager struct {
	mu        sync.Mutex
	historyMu sync.Mutex
	source    *config.ConfigManager
	current   string
}

func NewManager(source *config.ConfigManager) (*Manager, error) {
	if source == nil {
		return nil, errors.New("settings manager requires a config source manager")
	}
	return &Manager{source: source}, nil
}

func (m *Manager) Schema() map[string]any { return spec.Metadata() }

func (m *Manager) Snapshot(ctx context.Context) (Snapshot, error) {
	if m == nil || m.source == nil {
		return Snapshot{}, errors.New("settings manager is unavailable")
	}
	snap, err := m.source.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	m.historyMu.Lock()
	revision, baselineErr := m.ensureBaseline(snap)
	m.historyMu.Unlock()
	if baselineErr != nil {
		return Snapshot{}, baselineErr
	}
	warnings := unknownFieldWarnings(snap.Config)
	sourceYAML := make(map[string]string, len(snap.Sources))
	files, readErr := m.source.ReadSources(ctx)
	if readErr != nil {
		return Snapshot{}, readErr
	}
	for path, data := range files.Data {
		if redacted, redactErr := config.RedactYAMLForSettings(data); redactErr == nil {
			sourceYAML[path] = string(redacted)
		}
	}
	return Snapshot{
		Revision:       revision,
		ETag:           snap.ETag,
		Config:         snap.Config,
		YAML:           snap.YAML,
		Sources:        snap.Sources,
		SourceYAML:     sourceYAML,
		Ownership:      snap.Ownership,
		Writable:       snap.Writable,
		PendingRestart: snap.RestartRequired,
		RestartPaths:   append([]string(nil), snap.RestartPaths...),
		SchemaVersion:  1,
		Warnings:       warnings,
	}, nil
}

func unknownFieldWarnings(raw json.RawMessage) []Diagnostic {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	known := spec.KnownTopLevel()
	keys := make([]string, 0)
	for key := range value {
		if _, ok := known[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	warnings := make([]Diagnostic, 0, len(keys))
	for _, key := range keys {
		warnings = append(warnings, Diagnostic{Code: "unknown_field", Path: "/" + key, Severity: "warning", Message: "未识别字段已保留，结构化表单不会覆盖它"})
	}
	return warnings
}

func (m *Manager) Preview(ctx context.Context, draft Draft) (Preview, error) {
	if m == nil || m.source == nil {
		return Preview{}, errors.New("settings manager is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snap, err := m.source.Snapshot(ctx)
	if err != nil {
		return Preview{}, err
	}
	result := Preview{Valid: true, BaseETag: snap.ETag, Diff: append([]Change(nil), draft.Changes...)}
	if draft.Mode == "" {
		draft.Mode = ModeStructured
	}
	switch draft.Mode {
	case ModeStructured:
		patch, convertErr := changesToPatch(draft.Changes)
		if convertErr != nil {
			result.Valid = false
			result.Diagnostics = []Diagnostic{{Code: "invalid_change", Severity: "error", Message: convertErr.Error()}}
			return m.finishPreview(result, draft), nil
		}
		validation, validateErr := m.source.ValidatePatch(ctx, patch)
		if validateErr != nil {
			return Preview{}, validateErr
		}
		result.Valid = validation.Valid
		result.Impacts = impactsFromPatch(validation)
		for _, issue := range validation.Issues {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "validation", Path: issue.Path, Severity: "error", Message: issue.Message})
		}
		result.Migration = legacyMigrationPaths(snap.Config)
		if len(result.Migration) > 0 {
			result.Diff = append(result.Diff, legacyMigrationChanges(snap.Config)...)
		}
	case ModeSources:
		if len(draft.Sources) == 0 {
			result.Valid = false
			result.Diagnostics = []Diagnostic{{Code: "empty_sources", Severity: "error", Message: "至少提供一个 YAML 源文件"}}
			return m.finishPreview(result, draft), nil
		}
		updates := make(map[string][]byte, len(draft.Sources))
		for _, source := range draft.Sources {
			updates[source.SourceID] = []byte(source.YAML)
			result.SourceTarget = append(result.SourceTarget, source.SourceID)
		}
		validation, validateErr := m.source.ValidateSourceUpdates(ctx, updates)
		if validateErr != nil {
			return Preview{}, validateErr
		}
		if !validation.Valid {
			result.Valid = false
			for _, issue := range validation.Issues {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "yaml", Path: issue.Path, Severity: "error", Message: issue.Message})
			}
		}
	case ModeRestore:
		if strings.TrimSpace(draft.RestoreRevision) == "" {
			result.Valid = false
			result.Diagnostics = []Diagnostic{{Code: "missing_revision", Severity: "error", Message: "恢复操作需要 revision"}}
		} else if _, revisionErr := m.Revision(ctx, draft.RestoreRevision); revisionErr != nil {
			result.Valid = false
			result.Diagnostics = []Diagnostic{{Code: "unknown_revision", Severity: "error", Message: "历史 revision 不存在"}}
		}
	default:
		result.Valid = false
		result.Diagnostics = []Diagnostic{{Code: "invalid_mode", Severity: "error", Message: "不支持的配置草稿模式"}}
	}
	return m.finishPreview(result, draft), nil
}

func (m *Manager) Commit(ctx context.Context, draft Draft, etag, previewHash string, actor Actor) (CommitResult, error) {
	if m == nil || m.source == nil {
		return CommitResult{}, errors.New("settings manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	preview, err := m.Preview(ctx, draft)
	if err != nil {
		return CommitResult{}, err
	}
	if strings.TrimSpace(previewHash) == "" || preview.PreviewHash != previewHash {
		return CommitResult{Preview: preview}, fmt.Errorf("preview hash conflict")
	}
	if !preview.Valid {
		return CommitResult{Preview: preview}, fmt.Errorf("configuration draft is invalid")
	}
	if strings.TrimSpace(etag) == "" {
		return CommitResult{Preview: preview}, fmt.Errorf("If-Match is required")
	}
	var snap config.ConfigSnapshot
	switch draft.Mode {
	case "", ModeStructured:
		patch, convertErr := changesToPatch(preview.Diff)
		if convertErr != nil {
			return CommitResult{Preview: preview}, convertErr
		}
		snap, _, err = m.source.ApplyPatch(ctx, patch, etag)
	case ModeSources:
		updates := make(map[string][]byte, len(draft.Sources))
		for _, source := range draft.Sources {
			if strings.TrimSpace(source.SourceID) == "" {
				return CommitResult{Preview: preview}, errors.New("源文件路径不能为空")
			}
			updates[source.SourceID] = []byte(source.YAML)
		}
		snap, err = m.source.ApplySources(ctx, updates, etag)
	case ModeRestore:
		snap, err = m.restoreRevision(ctx, draft.RestoreRevision, etag)
	}
	if err != nil {
		return CommitResult{Preview: preview}, err
	}
	newSnapshot, snapshotErr := m.Snapshot(ctx)
	if snapshotErr != nil {
		return CommitResult{Preview: preview}, snapshotErr
	}
	newSnapshot.PendingRestart = snap.RestartRequired
	newSnapshot.RestartPaths = append([]string(nil), snap.RestartPaths...)
	revision, recordErr := m.recordRevision(snap, preview, actor, draft.Message)
	if recordErr != nil {
		return CommitResult{Snapshot: newSnapshot, Preview: preview}, recordErr
	}
	// Source snapshot filenames are private implementation details. They stay
	// in revision.json for restore but are never returned to API callers.
	revision.Sources = nil
	newSnapshot.Revision = revision.ID
	return CommitResult{Snapshot: newSnapshot, Revision: revision, Preview: preview}, nil
}

func changesToPatch(changes []Change) ([]byte, error) {
	if len(changes) == 0 {
		return nil, errors.New("至少提供一个配置变更")
	}
	ops := make([]config.PatchOp, 0, len(changes))
	for _, change := range changes {
		op := strings.ToLower(strings.TrimSpace(change.Op))
		if op == "reset" {
			op = "remove"
		}
		if op != "add" && op != "replace" && op != "remove" && op != "test" {
			return nil, fmt.Errorf("不支持的变更操作 %q", change.Op)
		}
		if strings.TrimSpace(change.Path) == "" {
			return nil, errors.New("配置路径不能为空")
		}
		opItem := config.PatchOp{Op: op, Path: change.Path, Value: change.Value}
		ops = append(ops, opItem)
	}
	return json.Marshal(ops)
}

func impactsFromPatch(validation config.PatchValidation) []Impact {
	impacts := make([]Impact, 0, 3)
	modelPaths := make([]string, 0)
	for _, operation := range validation.Diff {
		if strings.HasPrefix(operation.Path, "/models/") {
			modelPaths = append(modelPaths, operation.Path)
		}
	}
	if len(modelPaths) > 0 {
		impacts = append(impacts, Impact{Class: "model-restart", Paths: modelPaths})
	}
	if validation.RestartRequired {
		impacts = append(impacts, Impact{Class: "daemon-restart", Paths: append([]string(nil), validation.RestartPaths...)})
	}
	if len(impacts) == 0 {
		impacts = append(impacts, Impact{Class: "hot"})
	}
	return impacts
}

func legacyMigrationPaths(raw json.RawMessage) []string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	paths := make([]string, 0, 2)
	if _, ok := value["groups"]; ok {
		paths = append(paths, "/groups -> /routing/router/settings/groups")
	}
	if _, ok := value["matrix"]; ok {
		paths = append(paths, "/matrix -> /routing/router/settings/matrix")
	}
	return paths
}

func legacyMigrationChanges(raw json.RawMessage) []Change {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	legacyGroups, hasGroups := value["groups"]
	legacyMatrix, hasMatrix := value["matrix"]
	if !hasGroups && !hasMatrix {
		return nil
	}
	settings := map[string]any{}
	use := "group"
	if hasMatrix {
		use = "matrix"
		settings["matrix"] = legacyMatrix
	} else {
		settings["groups"] = legacyGroups
	}
	routing := map[string]any{"router": map[string]any{"use": use, "settings": settings}}
	op := "add"
	if _, exists := value["routing"]; exists {
		op = "replace"
	}
	changes := []Change{{Op: op, Path: "/routing", Value: routing}}
	if hasGroups {
		changes = append(changes, Change{Op: "remove", Path: "/groups"})
	}
	if hasMatrix {
		changes = append(changes, Change{Op: "remove", Path: "/matrix"})
	}
	return changes
}

func (m *Manager) finishPreview(result Preview, draft Draft) Preview {
	payload, _ := json.Marshal(struct {
		BaseETag string       `json:"baseEtag"`
		Draft    Draft        `json:"draft"`
		Valid    bool         `json:"valid"`
		Issues   []Diagnostic `json:"issues"`
	}{result.BaseETag, draft, result.Valid, result.Diagnostics})
	hash := sha256.Sum256(payload)
	result.PreviewHash = hex.EncodeToString(hash[:])
	return result
}

func (m *Manager) stateDir(sources []config.ConfigSource) string {
	root := ""
	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		if root == "" {
			root = filepath.Dir(source.Path)
		}
		paths = append(paths, source.Path)
	}
	if root == "" {
		root = "."
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		paths = []string{root}
	}
	h := sha256.Sum256([]byte(strings.Join(paths, "\x00")))
	return filepath.Join(root, ".llama-swap-config", hex.EncodeToString(h[:8]))
}

func (m *Manager) ensureBaseline(snap config.ConfigSnapshot) (string, error) {
	state := m.stateDir(snap.Sources)
	if err := os.MkdirAll(filepath.Join(state, "revisions"), 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(state, 0o700)
	_ = os.Chmod(filepath.Join(state, "revisions"), 0o700)
	entries, err := os.ReadDir(filepath.Join(state, "revisions"))
	if err == nil {
		var latest Revision
		found := false
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(state, "revisions", entry.Name(), "revision.json"))
			var candidate Revision
			if readErr != nil || json.Unmarshal(data, &candidate) != nil {
				continue
			}
			if !found || candidate.CreatedAt.After(latest.CreatedAt) {
				latest, found = candidate, true
			}
		}
		if found {
			m.current = latest.ID
			return m.current, nil
		}
	}
	base := Revision{ID: revisionID(snap), CreatedAt: time.Now().UTC(), Actor: Actor{ID: "startup", Origin: "startup"}, Origin: "startup", Result: "baseline"}
	if err := m.writeRevision(state, base, snap, nil); err != nil {
		return "", err
	}
	m.current = base.ID
	return base.ID, nil
}

func revisionID(snap config.ConfigSnapshot) string {
	h := sha256.Sum256([]byte(snap.ETag + snap.YAML))
	return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(h[:8]))
}

func (m *Manager) recordRevision(snap config.ConfigSnapshot, preview Preview, actor Actor, message string) (Revision, error) {
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	state := m.stateDir(snap.Sources)
	if err := os.MkdirAll(filepath.Join(state, "revisions"), 0o700); err != nil {
		return Revision{}, err
	}
	_ = os.Chmod(state, 0o700)
	_ = os.Chmod(filepath.Join(state, "revisions"), 0o700)
	id := revisionID(snap)
	result := "applied"
	if snap.RestartRequired {
		result = "applied-pending-restart"
	}
	revision := Revision{ID: id, Parent: m.current, CreatedAt: time.Now().UTC(), Actor: actor, Message: message, Origin: actor.Origin, Result: result, Impacts: preview.Impacts}
	for _, change := range preview.Diff {
		revision.Changed = append(revision.Changed, change.Path)
	}
	if err := m.writeRevision(state, revision, snap, &preview); err != nil {
		return Revision{}, err
	}
	// Retention is best-effort, but a silent failure lets the revision directory
	// — full config snapshots, secrets included — grow without bound while the
	// operator believes the retention policy is being enforced.
	if err := pruneHistory(state); err != nil {
		log.Printf("settings: prune revision history: %v", err)
	}
	m.current = id
	return revision, nil
}

func pruneHistory(state string) error {
	revisionsDir := filepath.Join(state, "revisions")
	entries, err := os.ReadDir(revisionsDir)
	if err != nil {
		return err
	}
	type item struct {
		dir      string
		revision Revision
		size     int64
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		manifestPath := filepath.Join(revisionsDir, entry.Name(), "revision.json")
		data, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			continue
		}
		var revision Revision
		if json.Unmarshal(data, &revision) != nil {
			continue
		}
		var size int64
		_ = filepath.Walk(filepath.Join(revisionsDir, entry.Name()), func(_ string, info os.FileInfo, walkErr error) error {
			if walkErr == nil && info != nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
		items = append(items, item{dir: filepath.Join(revisionsDir, entry.Name()), revision: revision, size: size})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].revision.CreatedAt.After(items[j].revision.CreatedAt) })
	cutoff := time.Now().UTC().Add(-historyMaxAge)
	var total int64
	for _, current := range items {
		total += current.size
	}
	for index, current := range items {
		keep := index < historyMinimum || current.revision.CreatedAt.After(cutoff)
		if keep || total <= historySoftCap {
			continue
		}
		if err := os.RemoveAll(current.dir); err != nil {
			return err
		}
		total -= current.size
	}
	return nil
}

func (m *Manager) writeRevision(state string, revision Revision, snap config.ConfigSnapshot, preview *Preview) error {
	dir := filepath.Join(state, "revisions", revision.ID)
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o700); err != nil {
		return err
	}
	revision.Sources = make(map[string]string)
	for _, source := range snap.Sources {
		data, err := os.ReadFile(source.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		h := sha256.Sum256([]byte(source.Path))
		name := hex.EncodeToString(h[:]) + ".yaml"
		if err := os.WriteFile(filepath.Join(dir, "sources", name), data, 0o600); err != nil {
			return err
		}
		revision.Sources[source.Path] = filepath.Join("sources", name)
	}
	manifest := revision
	manifest.SnapshotDir = dir
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "revision.json"), data, 0o600); err != nil {
		return err
	}
	if preview != nil {
		redacted, _ := json.Marshal(redactPreview(*preview))
		if err := os.WriteFile(filepath.Join(dir, "diff.json"), redacted, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Revision returns one manifest without exposing private source bytes.
func (m *Manager) Revision(ctx context.Context, id string) (Revision, error) {
	if strings.TrimSpace(id) == "" {
		return Revision{}, errors.New("revision id is required")
	}
	snap, err := m.Snapshot(ctx)
	if err != nil {
		return Revision{}, err
	}
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	data, err := os.ReadFile(filepath.Join(m.stateDir(snap.Sources), "revisions", filepath.Base(id), "revision.json"))
	if err != nil {
		return Revision{}, err
	}
	var revision Revision
	if err := json.Unmarshal(data, &revision); err != nil {
		return Revision{}, err
	}
	revision.SnapshotDir = filepath.Join(m.stateDir(snap.Sources), "revisions", filepath.Base(id))
	return revision, nil
}

func (m *Manager) restoreRevision(ctx context.Context, id, etag string) (config.ConfigSnapshot, error) {
	revision, err := m.Revision(ctx, id)
	if err != nil {
		return config.ConfigSnapshot{}, err
	}
	current, err := m.source.ReadSources(ctx)
	if err != nil {
		return config.ConfigSnapshot{}, err
	}
	updates := make(map[string][]byte)
	for path := range current.Data {
		// Emptying a source removes it from the effective merge while keeping
		// the file and its ownership visible to the source manager.
		updates[path] = []byte{}
	}
	for path, relative := range revision.Sources {
		data, readErr := os.ReadFile(filepath.Join(revision.SnapshotDir, relative))
		if readErr != nil {
			return config.ConfigSnapshot{}, readErr
		}
		updates[path] = data
	}
	return m.source.ApplySources(ctx, updates, etag)
}

func redactPreview(preview Preview) Preview {
	for i := range preview.Diff {
		if sensitivePath(preview.Diff[i].Path) {
			preview.Diff[i].Value = "[REDACTED]"
		}
	}
	return preview
}

func sensitivePath(path string) bool {
	lower := strings.ToLower(path)
	for _, token := range []string{"secret", "password", "token", "apikey", "api_key", "/env"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func (m *Manager) History(ctx context.Context, limit int) ([]Revision, error) {
	snap, err := m.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	dir := filepath.Join(m.stateDir(snap.Sources), "revisions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := make([]Revision, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name(), "revision.json"))
		if readErr != nil {
			continue
		}
		var revision Revision
		if json.Unmarshal(data, &revision) == nil {
			result = append(result, revision)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
