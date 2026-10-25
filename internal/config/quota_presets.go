package config

import (
	"embed"
	"fmt"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

// quotaPresetsFS carries the built-in quota mappings. Adding a plan is a data
// change: drop a YAML file in quotapresets/ and it becomes selectable as
// providers.<id>.quota.preset.
//
//go:embed quotapresets/*.yaml
var quotaPresetsFS embed.FS

var (
	quotaPresetsOnce  sync.Once
	quotaPresetsCache []QuotaPreset
	quotaPresetsErr   error
)

// QuotaPresets loads and validates every built-in quota preset once.
func QuotaPresets() ([]QuotaPreset, error) {
	quotaPresetsOnce.Do(func() {
		entries, err := quotaPresetsFS.ReadDir("quotapresets")
		if err != nil {
			quotaPresetsErr = fmt.Errorf("reading quota presets: %w", err)
			return
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)

		presets := make([]QuotaPreset, 0, len(names))
		for _, name := range names {
			data, err := quotaPresetsFS.ReadFile(path.Join("quotapresets", name))
			if err != nil {
				quotaPresetsErr = fmt.Errorf("reading quota preset %s: %w", name, err)
				return
			}
			var preset QuotaPreset
			if err := yaml.Unmarshal(data, &preset); err != nil {
				quotaPresetsErr = fmt.Errorf("quota preset %s: %w", name, err)
				return
			}
			if err := preset.normalize(name); err != nil {
				quotaPresetsErr = fmt.Errorf("quota preset %s: %w", name, err)
				return
			}
			presets = append(presets, preset)
		}
		quotaPresetsCache = presets
	})
	return quotaPresetsCache, quotaPresetsErr
}

// normalize validates a preset by running it through the same code path a
// provider's inline quota block uses.
func (p *QuotaPreset) normalize(file string) error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return fmt.Errorf("%s: id is required", file)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = p.ID
	}
	p.Description = strings.TrimSpace(p.Description)
	p.Category = strings.TrimSpace(p.Category)
	// Validate the mapping without a preset reference, and without touching
	// the caller's enforce section (none in a preset).
	candidate := QuotaConfig{
		Endpoint:       p.Endpoint,
		Method:         p.Method,
		Auth:           p.Auth,
		Headers:        p.Headers,
		TimeoutSeconds: p.TimeoutSeconds,
		DivideBy:       p.DivideBy,
		Windows:        p.Windows,
		Extras:         p.Extras,
		Script:         p.Script,
	}
	if err := candidate.normalize(); err != nil {
		return err
	}
	p.Endpoint, p.Method, p.Auth = candidate.Endpoint, candidate.Method, candidate.Auth
	p.Headers, p.TimeoutSeconds = candidate.Headers, candidate.TimeoutSeconds
	p.Windows, p.Extras = candidate.Windows, candidate.Extras
	p.DivideBy = candidate.DivideBy
	p.Script = candidate.Script
	return nil
}

// QuotaPresetSummary is the metadata the UI needs to offer a preset in its
// dropdown: the id it stores, the label it shows, and what the mapping does.
type QuotaPresetSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	// NeedsBase marks an endpoint expressed as "{base}/path": the effective
	// URL comes from the provider's own endpoint.
	NeedsBase bool `json:"needsBase,omitempty"`
	// HasScript marks a preset driven by JavaScript: the editor offers to
	// load that script so it can be adapted per provider.
	HasScript bool `json:"hasScript,omitempty"`
	Windows   int  `json:"windows"`
	Extras    int  `json:"extras"`
}

// QuotaPresetDetail is a preset including its script source.
type QuotaPresetDetail struct {
	QuotaPresetSummary
	Script string `json:"script,omitempty"`
}

// QuotaPresetSummaries lists the built-in presets for the control API.
func QuotaPresetSummaries() []QuotaPresetSummary {
	presets, err := QuotaPresets()
	if err != nil {
		return nil
	}
	out := make([]QuotaPresetSummary, 0, len(presets))
	for _, preset := range presets {
		out = append(out, summarizeQuotaPreset(preset))
	}
	return out
}

// QuotaPresetDetailByID returns one preset with its script source.
func QuotaPresetDetailByID(id string) (QuotaPresetDetail, bool) {
	preset, ok := QuotaPresetByID(id)
	if !ok {
		return QuotaPresetDetail{}, false
	}
	return QuotaPresetDetail{QuotaPresetSummary: summarizeQuotaPreset(preset), Script: preset.Script}, true
}

func summarizeQuotaPreset(preset QuotaPreset) QuotaPresetSummary {
	return QuotaPresetSummary{
		ID:          preset.ID,
		Name:        preset.Name,
		Description: preset.Description,
		Category:    preset.Category,
		Endpoint:    preset.Endpoint,
		NeedsBase:   strings.HasPrefix(preset.Endpoint, QuotaBasePlaceholder),
		HasScript:   strings.TrimSpace(preset.Script) != "",
		Windows:     len(preset.Windows),
		Extras:      len(preset.Extras),
	}
}

func validHeaderName(name string) bool {
	return httpguts.ValidHeaderFieldName(name)
}

func canonicalHeaderName(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}
