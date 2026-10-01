package extensions

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Presets are packaged extensions the Extensions UI offers as a store. They are
// embedded in the binary so the catalog works on a machine with no network
// access, and installing one is nothing more than saving its files as a normal
// extension: the user owns that copy afterwards and can edit or delete it.
//
// The embed deliberately omits "all:", so a tool that drops its own dotfile
// into the tree does not become a preset.
//
//go:embed presets
var presetFS embed.FS

// storeMetaFile is store-only metadata for a preset. It is deliberately not
// part of the extension's manifest, and it is never copied into an installed
// extension.
const storeMetaFile = "store.yaml"

// presetCategoryOrder drives the order the store lists categories in.
var presetCategoryOrder = []string{"search", "documents", "knowledge", "memory", "other"}

type storeMeta struct {
	Category string   `json:"category" yaml:"category"`
	Tags     []string `json:"tags" yaml:"tags"`
}

// Preset is one store entry: its manifest, its files, and the summary the store
// needs to render a card before the user installs it.
type Preset struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Category    string            `json:"category"`
	Tags        []string          `json:"tags,omitempty"`
	Settings    []SettingField    `json:"settings,omitempty"`
	Hosts       []string          `json:"hosts"`
	Tools       []string          `json:"tools"`
	Manifest    Manifest          `json:"manifest"`
	Files       map[string]string `json:"files"`
}

var (
	presetOnce   sync.Once
	presetCache  []Preset
	presetIssues []string
)

// Presets returns every embedded preset, ordered by category then by id. The
// catalog is compiled once per process: it is embedded data, and compiling it
// also proves each preset actually loads.
func Presets() []Preset {
	presetOnce.Do(func() {
		presetCache, presetIssues = loadPresets()
	})
	return presetCache
}

// presetProblems reports why any preset was left out of the catalog. It is
// empty in a healthy build and is what the build test asserts on.
func presetProblems() []string {
	Presets()
	return presetIssues
}

func loadPresets() ([]Preset, []string) {
	entries, err := fs.ReadDir(presetFS, "presets")
	if err != nil {
		return nil, []string{err.Error()}
	}
	var presets []Preset
	var problems []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		preset, err := loadPreset(entry.Name())
		if err != nil {
			// A broken preset must not take the whole catalog down: the store
			// simply does not offer it.
			problems = append(problems, fmt.Sprintf("preset %s: %v", entry.Name(), err))
			continue
		}
		presets = append(presets, preset)
	}
	sort.SliceStable(presets, func(i, j int) bool {
		left, right := categoryRank(presets[i].Category), categoryRank(presets[j].Category)
		if left != right {
			return left < right
		}
		return presets[i].ID < presets[j].ID
	})
	return presets, problems
}

func categoryRank(category string) int {
	for index, candidate := range presetCategoryOrder {
		if candidate == category {
			return index
		}
	}
	return len(presetCategoryOrder)
}

// PresetByID returns one preset by its id.
func PresetByID(id string) (Preset, bool) {
	for _, preset := range Presets() {
		if preset.ID == id {
			return preset, true
		}
	}
	return Preset{}, false
}

func loadPreset(id string) (Preset, error) {
	if !safeID(id) {
		return Preset{}, fmt.Errorf("%q is not a valid preset id", id)
	}
	files, err := readTreeFS(presetFS, path.Join("presets", id))
	if err != nil {
		return Preset{}, err
	}
	manifestBytes, ok := files[ManifestFile]
	if !ok {
		return Preset{}, fmt.Errorf("no %s", ManifestFile)
	}
	var manifest Manifest
	if err := yaml.Unmarshal([]byte(manifestBytes), &manifest); err != nil {
		return Preset{}, err
	}
	if manifest.Name == "" {
		manifest.Name = id
	}
	manifest.ID = id
	if err := manifest.Validate(); err != nil {
		return Preset{}, err
	}
	delete(files, ManifestFile)
	meta := storeMeta{}
	if raw, ok := files[storeMetaFile]; ok {
		delete(files, storeMetaFile)
		_ = yaml.Unmarshal([]byte(raw), &meta)
	}
	// Everything else about the preset is checked against the real compiler:
	// a preset whose scripts do not bundle, or whose settings declaration is
	// malformed, is reported instead of sold to the user.
	compiled, err := inspectTree(context.Background(), files)
	if err != nil {
		return Preset{}, err
	}
	category := meta.Category
	if category == "" {
		category = presetCategory(id)
	}
	return Preset{
		ID:          id,
		Name:        manifest.Name,
		Description: manifest.Description,
		Category:    category,
		Tags:        meta.Tags,
		Settings:    compiled.Settings,
		Hosts:       manifest.Permissions.NetworkHosts,
		Tools:       compiled.ToolNames(),
		Manifest:    manifest,
		Files:       files,
	}, nil
}

// inspectTree bundles a tree in memory and inspects it, which is how the store
// learns a preset's declared settings and tools without installing it.
func inspectTree(ctx context.Context, files map[string]string) (*Compiled, error) {
	bundle, err := bundleTree(files)
	if err != nil {
		return nil, err
	}
	compiled := &Compiled{Bundle: bundle}
	if err := compiled.Inspect(ctx); err != nil {
		return nil, err
	}
	return compiled, nil
}

// presetCategory guesses a category from the preset id, so a new preset still
// lands in a sensible group when it ships without a store.yaml.
func presetCategory(id string) string {
	switch {
	case strings.HasPrefix(id, "web-search-"), id == "openai-web-search":
		return "search"
	case strings.HasPrefix(id, "pdf-"):
		return "documents"
	case strings.HasPrefix(id, "rag-"):
		return "knowledge"
	case strings.Contains(id, "memory"):
		return "memory"
	default:
		return "other"
	}
}

// readTreeFS is readTree for an embedded filesystem.
func readTreeFS(fsys fs.FS, root string) (map[string]string, error) {
	files := map[string]string{}
	total := 0
	err := fs.WalkDir(fsys, root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if current == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(current, root), "/")
		if !managedExtensions[path.Ext(relative)] {
			return nil
		}
		data, readErr := fs.ReadFile(fsys, current)
		if readErr != nil {
			return readErr
		}
		if len(data) > MaxExtensionFileBytes {
			return fmt.Errorf("file %q is too large", relative)
		}
		total += len(data)
		if total > MaxExtensionTreeBytes {
			return errors.New("files are too large")
		}
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// InstallPreset copies a preset into the extension directory as a new
// extension. It refuses to overwrite an extension that already exists: the
// store offers starting points, and an installed copy belongs to the user.
func (m *Manager) InstallPreset(ctx context.Context, preset Preset, targetID string) (Definition, error) {
	if targetID == "" {
		targetID = preset.ID
	}
	if !safeID(targetID) {
		return Definition{}, fmt.Errorf("%q is not a valid extension id", targetID)
	}
	if _, err := m.get(targetID); err == nil {
		return Definition{}, fmt.Errorf("extension %s already exists", targetID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Definition{}, err
	}
	settings := preset.Settings
	manifest := preset.Manifest
	manifest.ID = targetID
	manifest.Enabled = true
	// A credential the user has not supplied yet still has to satisfy the
	// declaration, so the install seeds it rather than refusing to install.
	for key, value := range seedRequiredSettings(settings) {
		if manifest.Config == nil {
			manifest.Config = map[string]any{}
		}
		if _, present := manifest.Config[key]; !present {
			manifest.Config[key] = value
		}
	}
	return m.Save(ctx, Definition{Manifest: manifest, Files: preset.Files}, "")
}
