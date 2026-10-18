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
	// Validate the mapping without a preset reference, and without touching
	// the caller's enforce section (none in a preset).
	candidate := QuotaConfig{
		Endpoint:       p.Endpoint,
		Method:         p.Method,
		Auth:           p.Auth,
		Headers:        p.Headers,
		TimeoutSeconds: p.TimeoutSeconds,
		Windows:        p.Windows,
		Extras:         p.Extras,
	}
	if err := candidate.normalize(); err != nil {
		return err
	}
	p.Endpoint, p.Method, p.Auth = candidate.Endpoint, candidate.Method, candidate.Auth
	p.Headers, p.TimeoutSeconds = candidate.Headers, candidate.TimeoutSeconds
	p.Windows, p.Extras = candidate.Windows, candidate.Extras
	return nil
}

func validHeaderName(name string) bool {
	return httpguts.ValidHeaderFieldName(name)
}

func canonicalHeaderName(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}
