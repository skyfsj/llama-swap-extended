package settings

import "testing"

// TestSensitivePath_TokenCounts covers the diff preview redaction: a token
// count is a number, and hiding it behind "[REDACTED]" makes the settings
// history unreadable while still not protecting anything.
func TestSensitivePath_TokenCounts(t *testing.T) {
	credential := []string{
		"apiKeys", "providers.openrouter.apiKeys.0.value",
		"/modelFiles/downloads/hfToken", "modelFiles.downloads.modelScopeToken",
		"models.m.env", "auth.access_token", "client_secret",
	}
	for _, path := range credential {
		if !sensitivePath(path) {
			t.Errorf("%s must stay redacted", path)
		}
	}
	count := []string{
		"models.m.filters.setParams.max_tokens",
		"/models/m/filters/setParams/max_output_tokens",
		"usage.input_tokens",
		"activity.output_tokens",
	}
	for _, path := range count {
		if sensitivePath(path) {
			t.Errorf("%s is a token count and must stay visible", path)
		}
	}
}

func TestLastPathSegment(t *testing.T) {
	cases := map[string]string{
		"models.m.filters.max_tokens":    "max_tokens",
		"/models/m/filters/max_tokens":   "max_tokens",
		"max_tokens":                     "max_tokens",
		"providers.x.apiKeys.0.value":    "value",
		"/modelFiles/downloads/hfToken/": "hfToken",
	}
	for path, want := range cases {
		if got := lastPathSegment(path); got != want {
			t.Errorf("lastPathSegment(%q) = %q, want %q", path, got, want)
		}
	}
}
