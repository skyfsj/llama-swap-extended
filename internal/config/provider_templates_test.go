package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProviderTemplateLoads(t *testing.T) {
	templates, err := ProviderTemplates()
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no built-in provider templates")
	}
	template, ok := ProviderTemplateByID("opencode-go")
	if !ok {
		t.Fatal("opencode-go template missing")
	}
	if template.ModelsEndpoint != "https://opencode.ai/inference/go/v1/models" {
		t.Errorf("models endpoint = %q", template.ModelsEndpoint)
	}
	if template.ModelsAuth != TemplateModelsAuthNone {
		t.Errorf("modelsAuth = %q", template.ModelsAuth)
	}
	if len(template.Adapters) != 4 {
		t.Fatalf("adapters = %d, want 4", len(template.Adapters))
	}
	if !reflect.DeepEqual(template.Provider.SessionHeaders, []string{"x-opencode-session"}) {
		t.Errorf("session headers = %v", template.Provider.SessionHeaders)
	}
	if template.Provider.Quota == nil || template.Provider.Quota.Preset != "opencode-go" {
		t.Errorf("quota = %+v", template.Provider.Quota)
	}
	google := template.Adapters[2]
	if google.Type != ProviderTypeGemini || google.PathMap["/v1beta/models/"] != "/v1/models/" {
		t.Errorf("google adapter = %+v", google)
	}
}

func TestProviderTemplateDistributesModels(t *testing.T) {
	template, ok := ProviderTemplateByID("opencode-go")
	if !ok {
		t.Fatal("opencode-go template missing")
	}
	assigned := template.DistributeModels([]string{
		"grok-4.7", "gpt-6-luna", "muse-spark-1.2-contributor",
		"claude-haiku-5-5", "minimax-m3", "qwen3.8-max",
		"gemini-4-argon-contributor",
		"kimi-k3", "glm-5.3", "brand-new-model-2099",
	})
	want := map[string][]string{
		"Responses": {"gpt-6-luna", "grok-4.7", "muse-spark-1.2-contributor"},
		"Messages":  {"claude-haiku-5-5", "minimax-m3", "qwen3.8-max"},
		"Google":    {"gemini-4-argon-contributor"},
		// Unknown models fall through to the default (catch-all) adapter.
		"Chat": {"brand-new-model-2099", "glm-5.3", "kimi-k3"},
	}
	if !reflect.DeepEqual(assigned, want) {
		t.Fatalf("distribution =\n%v\nwant\n%v", assigned, want)
	}
}

func TestProviderTemplateDraft(t *testing.T) {
	template, ok := ProviderTemplateByID("opencode-go")
	if !ok {
		t.Fatal("opencode-go template missing")
	}
	draft := template.ProviderDraft("sk-test", []string{"grok-4.7", "kimi-k3"})
	if draft["template"] != "opencode-go" {
		t.Errorf("template provenance = %v", draft["template"])
	}
	adapters, ok := draft["adapters"].([]map[string]any)
	if !ok || len(adapters) != 4 {
		t.Fatalf("adapters = %#v", draft["adapters"])
	}
	if models, _ := adapters[0]["models"].([]any); len(models) != 1 || models[0] != "grok-4.7" {
		t.Errorf("responses models = %#v", adapters[0]["models"])
	}
	if models, _ := adapters[3]["models"].([]any); len(models) != 1 || models[0] != "kimi-k3" {
		t.Errorf("chat models = %#v", adapters[3]["models"])
	}
	// The draft must satisfy the real provider validation, key included.
	data, err := yaml.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal draft: %v", err)
	}
	var provider ProviderConfig
	if err := yaml.Unmarshal(data, &provider); err != nil {
		t.Fatalf("unmarshal draft: %v", err)
	}
	if len(provider.EffectiveAdapters()) != 4 || len(provider.AllModels()) != 2 {
		t.Fatalf("round-tripped draft = %+v", provider)
	}
	if len(provider.ApiKeys) != 1 || provider.ApiKeys[0].Value != "sk-test" {
		t.Fatalf("draft credentials = %+v", provider.ApiKeys)
	}
	if provider.Template != "opencode-go" || provider.Quota == nil || provider.Quota.Preset != "opencode-go" {
		t.Fatalf("draft provenance/quota = %q %+v", provider.Template, provider.Quota)
	}
}
