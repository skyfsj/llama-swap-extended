package config

import (
	"embed"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Provider templates describe a known upstream subscription so the UI can add
// it in one step: the four API surfaces, the session header the provider
// expects, and the usage mapping. Model lists are deliberately NOT part of a
// template: they change often, so applying a template pulls the live model
// list and distributes it across the adapters with each adapter's
// modelMatch patterns (see config.DistributeModels).
//
//go:embed providertemplates/*.yaml
var providerTemplatesFS embed.FS

var (
	providerTemplatesOnce  sync.Once
	providerTemplatesCache []ProviderTemplate
	providerTemplatesErr   error
)

// ProviderTemplate is one built-in subscription template.
type ProviderTemplate struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// ModelsEndpoint is the absolute URL listing the subscription's models.
	ModelsEndpoint string `yaml:"modelsEndpoint"`
	// ModelsAuth selects how a credential is presented when pulling models:
	// "bearer" (default), "x-api-key", "x-goog-api-key" or "none".
	ModelsAuth string `yaml:"modelsAuth"`
	// Provider carries the provider-level settings of the created entry.
	Provider TemplateProvider `yaml:"provider"`
	// Adapters define the API surfaces; each must carry at least one
	// modelMatch pattern, and exactly one adapter sets default: true.
	Adapters []ProviderAdapter `yaml:"adapters"`
}

// TemplateProvider is the provider skeleton a template seeds.
type TemplateProvider struct {
	SessionHeaders []string          `yaml:"sessionHeaders"`
	Headers        map[string]string `yaml:"headers"`
	Sticky         *bool             `yaml:"sticky"`
	BareNamePolicy string            `yaml:"bareNamePolicy"`
	MaxRetries     *int              `yaml:"maxRetries"`
	Proxy          string            `yaml:"proxy"`
	Quota          *QuotaConfig      `yaml:"quota"`
}

// ModelsAuth values.
const (
	TemplateModelsAuthBearer = "bearer"
	TemplateModelsAuthAPIKey = "x-api-key"
	TemplateModelsAuthGoogle = "x-goog-api-key"
	TemplateModelsAuthNone   = "none"
)

// ProviderTemplates loads and validates every built-in template once.
func ProviderTemplates() ([]ProviderTemplate, error) {
	providerTemplatesOnce.Do(func() {
		entries, err := providerTemplatesFS.ReadDir("providertemplates")
		if err != nil {
			providerTemplatesErr = fmt.Errorf("reading provider templates: %w", err)
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

		templates := make([]ProviderTemplate, 0, len(names))
		for _, name := range names {
			data, err := providerTemplatesFS.ReadFile(path.Join("providertemplates", name))
			if err != nil {
				providerTemplatesErr = fmt.Errorf("reading provider template %s: %w", name, err)
				return
			}
			var template ProviderTemplate
			if err := yaml.Unmarshal(data, &template); err != nil {
				providerTemplatesErr = fmt.Errorf("provider template %s: %w", name, err)
				return
			}
			if err := template.normalize(name); err != nil {
				providerTemplatesErr = fmt.Errorf("provider template %s: %w", name, err)
				return
			}
			templates = append(templates, template)
		}
		providerTemplatesCache = templates
	})
	return providerTemplatesCache, providerTemplatesErr
}

// ProviderTemplateByID resolves a built-in template.
func ProviderTemplateByID(id string) (ProviderTemplate, bool) {
	templates, err := ProviderTemplates()
	if err != nil {
		return ProviderTemplate{}, false
	}
	for _, template := range templates {
		if template.ID == id {
			return template, true
		}
	}
	return ProviderTemplate{}, false
}

// normalize validates a template by running its adapters through the same
// checks a real provider config gets, minus the model requirement (models are
// pulled at apply time).
func (t *ProviderTemplate) normalize(file string) error {
	t.ID = strings.TrimSpace(t.ID)
	if t.ID == "" {
		return fmt.Errorf("%s: id is required", file)
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		t.Name = t.ID
	}
	t.ModelsEndpoint = strings.TrimSpace(t.ModelsEndpoint)
	if t.ModelsEndpoint == "" {
		return fmt.Errorf("%s: modelsEndpoint is required", file)
	}
	if err := validateAbsoluteHTTPURL(t.ModelsEndpoint, "modelsEndpoint"); err != nil {
		return err
	}
	switch t.ModelsAuth {
	case "":
		t.ModelsAuth = TemplateModelsAuthBearer
	case TemplateModelsAuthBearer, TemplateModelsAuthAPIKey, TemplateModelsAuthGoogle, TemplateModelsAuthNone:
	default:
		return fmt.Errorf("%s: modelsAuth must be %q, %q, %q or %q, got %q", file,
			TemplateModelsAuthBearer, TemplateModelsAuthAPIKey, TemplateModelsAuthGoogle, TemplateModelsAuthNone, t.ModelsAuth)
	}

	if len(t.Adapters) == 0 {
		return fmt.Errorf("%s: at least one adapter is required", file)
	}
	defaults := 0
	for i := range t.Adapters {
		label := fmt.Sprintf("adapters[%d]", i)
		if name := strings.TrimSpace(t.Adapters[i].Name); name != "" {
			label = fmt.Sprintf("adapter %q", name)
			t.Adapters[i].Name = name
		}
		if len(t.Adapters[i].Models) > 0 {
			return fmt.Errorf("%s: %s must not pre-set models; they are pulled when the template is applied", file, label)
		}
		if len(t.Adapters[i].ApiKeys) > 0 {
			return fmt.Errorf("%s: %s must not pre-set apiKeys; the created provider owns its credentials", file, label)
		}
		if len(t.Adapters[i].ModelMatch) == 0 {
			return fmt.Errorf("%s: %s needs modelMatch patterns", file, label)
		}
		// Reuse the provider normalization (endpoint/type/pathMap/modelMatch
		// checks); the receiver only supplies weight defaults, which a
		// template does not set.
		validator := ProviderConfig{}
		if err := validator.normalizeAdapter(label, &t.Adapters[i]); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if t.Adapters[i].Default {
			defaults++
		}
	}
	if defaults != 1 {
		return fmt.Errorf("%s: exactly one adapter must set default: true (got %d)", file, defaults)
	}

	if headers := t.Provider.Headers; len(headers) > 0 {
		normalized := make(map[string]string, len(headers))
		if err := normalizeStaticHeaders(normalized, headers); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		t.Provider.Headers = normalized
	}
	if err := t.Provider.Quota.normalize(); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// DistributeModels assigns pulled model ids to template adapters: the first
// adapter whose modelMatch glob accepts the id wins, everything else lands on
// the default adapter.
func (t ProviderTemplate) DistributeModels(models []string) map[string][]string {
	assigned := make(map[string][]string, len(t.Adapters))
	for _, model := range models {
		target := ""
		for _, adapter := range t.Adapters {
			for _, pattern := range adapter.ModelMatch {
				if matched, err := path.Match(pattern, model); err == nil && matched {
					target = adapterLabel(adapter)
					break
				}
			}
			if target != "" {
				break
			}
		}
		if target == "" {
			for _, adapter := range t.Adapters {
				if adapter.Default {
					target = adapterLabel(adapter)
					break
				}
			}
		}
		if target == "" {
			continue
		}
		assigned[target] = append(assigned[target], model)
	}
	for label := range assigned {
		sort.Strings(assigned[label])
	}
	return assigned
}

// ProviderDraft builds the provider entry a template creates: the adapters
// with the pulled models distributed by modelMatch, and a single credential.
// apiKey may be empty; the UI collects it before saving.
func (t ProviderTemplate) ProviderDraft(apiKey string, models []string) map[string]any {
	assigned := t.DistributeModels(models)
	adapters := make([]map[string]any, 0, len(t.Adapters))
	for _, adapter := range t.Adapters {
		entries := make([]any, 0, len(assigned[adapterLabel(adapter)]))
		for _, model := range assigned[adapterLabel(adapter)] {
			entries = append(entries, model)
		}
		entry := map[string]any{
			"type":     adapter.Type,
			"endpoint": adapter.Endpoint,
			"models":   entries,
		}
		if adapter.Name != "" {
			entry["name"] = adapter.Name
		}
		if len(adapter.PathMap) > 0 {
			entry["pathMap"] = adapter.PathMap
		}
		if len(adapter.ModelMatch) > 0 {
			entry["modelMatch"] = adapter.ModelMatch
		}
		if adapter.Default {
			entry["default"] = true
		}
		adapters = append(adapters, entry)
	}
	provider := map[string]any{
		"adapters": adapters,
		"template": t.ID,
		"apiKeys":  []any{map[string]any{"value": apiKey}},
	}
	if len(t.Provider.SessionHeaders) > 0 {
		provider["sessionHeaders"] = t.Provider.SessionHeaders
	}
	if len(t.Provider.Headers) > 0 {
		provider["headers"] = t.Provider.Headers
	}
	if t.Provider.Proxy != "" {
		provider["proxy"] = t.Provider.Proxy
	}
	if t.Provider.BareNamePolicy != "" {
		provider["bareNamePolicy"] = t.Provider.BareNamePolicy
	}
	if t.Provider.MaxRetries != nil {
		provider["maxRetries"] = *t.Provider.MaxRetries
	}
	if t.Provider.Sticky != nil {
		provider["sticky"] = *t.Provider.Sticky
	}
	if t.Provider.Quota != nil {
		quota := map[string]any{}
		if t.Provider.Quota.Preset != "" {
			quota["preset"] = t.Provider.Quota.Preset
		}
		if t.Provider.Quota.Enforce.Enabled {
			quota["enforce"] = map[string]any{"enabled": true}
			if t.Provider.Quota.Enforce.Threshold != 0 {
				quota["enforce"].(map[string]any)["threshold"] = t.Provider.Quota.Enforce.Threshold
			}
		}
		provider["quota"] = quota
	}
	return provider
}

func adapterLabel(adapter ProviderAdapter) string {
	if adapter.Name != "" {
		return adapter.Name
	}
	return adapter.Type
}

// validateAbsoluteHTTPURL checks a configured absolute URL.
func validateAbsoluteHTTPURL(raw, field string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL (%s): %w", field, raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must be an absolute http(s) URL, got %q", field, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s is missing a host", field)
	}
	return nil
}
