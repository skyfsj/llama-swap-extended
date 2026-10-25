package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigManagerSnapshotKeepsTokenCounts covers the defect where every key
// mentioning "token" was treated as credential material. A token *count* is an
// operational number: redacting it hands the settings editor and any client
// reading the config a string where a limit belongs, and a client that sends
// that placeholder back as max_tokens makes the upstream reject the request.
func TestConfigManagerSnapshotKeepsTokenCounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `models:
    m:
        cmd: llama-server --port ${PORT}
        filters:
            setParams:
                max_tokens: 4096
                max_output_tokens: 8192
                temperature: 0.3
                api_key: sk-should-be-redacted
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_tokens: 4096", "max_output_tokens: 8192", "temperature: 0.3"} {
		if !strings.Contains(snapshot.YAML, want) {
			t.Errorf("token count %q was redacted:\n%s", want, snapshot.YAML)
		}
	}
	// A real credential under a credential-shaped name still redacts.
	if strings.Contains(snapshot.YAML, "sk-should-be-redacted") {
		t.Errorf("api_key was not redacted:\n%s", snapshot.YAML)
	}
	if !strings.Contains(snapshot.YAML, "api_key: '[REDACTED]'") && !strings.Contains(snapshot.YAML, `api_key: "[REDACTED]"`) {
		t.Errorf("api_key did not carry the redaction sentinel:\n%s", snapshot.YAML)
	}
}

// TestIsSensitiveConfigName_TokenCounts pins the rule itself, including the
// names that must stay credentials.
func TestIsSensitiveConfigName_TokenCounts(t *testing.T) {
	credential := []string{
		"token", "access_token", "auth_token", "refresh_token",
		"api_key", "apikey", "apiKey", "password", "secret",
		"client_secret", "token_secret", "hf_token", "hfToken",
		"modelScopeToken", "github_token", "credential",
	}
	for _, name := range credential {
		if !IsSensitiveConfigName(name) {
			t.Errorf("%s must be treated as a credential", name)
		}
	}
	count := []string{
		"max_tokens", "max_output_tokens", "max_completion_tokens",
		"maxInputTokens", "input_tokens", "output_tokens", "total_tokens",
		"prompt_tokens", "completion_tokens", "cached_tokens",
		"reasoning_tokens", "budget_tokens", "thinking_budget_tokens",
		"max_any_future_tokens",
	}
	for _, name := range count {
		if IsSensitiveConfigName(name) {
			t.Errorf("%s is a token count, not a credential", name)
		}
	}
}
