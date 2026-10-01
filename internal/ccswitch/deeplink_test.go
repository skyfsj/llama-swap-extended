package ccswitch

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuildProviderOmitsKeyByDefault(t *testing.T) {
	link, err := BuildProvider(Provider{App: "codex", Name: "local", Endpoint: "http://127.0.0.1:8080/v1"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("apiKey") != "" || u.Query().Get("resource") != "provider" || u.Query().Get("app") != "codex" {
		t.Fatalf("link=%s", link)
	}
}

func TestBuildProviderIncludesKeyOnlyWhenRequested(t *testing.T) {
	link, err := BuildProvider(Provider{App: "claude", Name: "p", Endpoint: "https://x", APIKey: "secret", IncludeKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := url.Parse(link); got.Query().Get("apiKey") != "secret" {
		t.Fatalf("link=%s", link)
	}
}

func TestBuildProviderSupportsGeminiAndClaudeModelOverrides(t *testing.T) {
	claude, err := BuildProvider(Provider{
		App:         "claude",
		Name:        "local",
		Endpoint:    "https://x/v1",
		Model:       "main-model",
		HaikuModel:  "haiku-model",
		SonnetModel: "sonnet-model",
		OpusModel:   "opus-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeURL, err := url.Parse(claude)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := claudeURL.Query().Get("haikuModel"), "haiku-model"; got != want {
		t.Fatalf("haikuModel=%q, want %q", got, want)
	}
	if got, want := claudeURL.Query().Get("sonnetModel"), "sonnet-model"; got != want {
		t.Fatalf("sonnetModel=%q, want %q", got, want)
	}
	if got, want := claudeURL.Query().Get("opusModel"), "opus-model"; got != want {
		t.Fatalf("opusModel=%q, want %q", got, want)
	}

	gemini, err := BuildProvider(Provider{App: "gemini", Name: "local", Endpoint: "https://x/v1", Model: "gemini-model"})
	if err != nil {
		t.Fatal(err)
	}
	geminiURL, err := url.Parse(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if got := geminiURL.Query().Get("app"); got != "gemini" {
		t.Fatalf("gemini app=%q", got)
	}
	if got := geminiURL.Query().Get("model"); got != "gemini-model" {
		t.Fatalf("gemini model=%q", got)
	}
}

func TestBuildProviderNormalizesSingleEndpoint(t *testing.T) {
	link, err := BuildProvider(Provider{
		App:      "codex",
		Name:     "local",
		Endpoint: " https://local.example/v1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Query().Get("endpoint"), "https://local.example/v1"; got != want {
		t.Fatalf("endpoint=%q, want %q", got, want)
	}
}

func TestBuildProviderRejectsMalformedOrMultipleEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://primary.example,https://backup.example",
		"https://primary.example,",
		"ftp://primary.example",
		"https://user:secret@primary.example",
		"https://primary.example/v1?api_key=secret",
		"https://primary.example/v1#fragment",
		"https://primary.example/v1 backup",
		"https://primary.example\n",
		"https://primary.example/\u200b",
	} {
		if _, err := BuildProvider(Provider{App: "claude", Name: "local", Endpoint: endpoint}); err == nil {
			t.Fatalf("endpoint list %q was accepted", endpoint)
		}
	}
}

func TestBuildProviderRejectsInvisibleProviderFields(t *testing.T) {
	for _, provider := range []Provider{
		{App: "codex", Name: "local\u200b", Endpoint: "https://x"},
		{App: "codex", Name: "local", Model: "model\u00a0name", Endpoint: "https://x"},
		{App: "codex", Name: "local", Endpoint: "https://x", IncludeKey: true, APIKey: "secret\nvalue"},
	} {
		if _, err := BuildProvider(provider); err == nil {
			t.Fatalf("provider with invisible/control field was accepted: %+v", provider)
		}
	}
	if _, err := BuildProvider(Provider{App: "codex", Name: strings.Repeat("x", 4), Endpoint: "https://x", Model: "model name"}); err != nil {
		t.Fatalf("ordinary ASCII spaces should remain valid in display/model fields: %v", err)
	}
}
