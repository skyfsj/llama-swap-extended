package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_RequestLocaleResolvesConsoleLanguage(t *testing.T) {
	cases := map[string]string{
		"":                                   "zh-CN",
		"en-US,en;q=0.9":                     "en",
		"zh-TW,zh;q=0.8,en;q=0.7":            "zh-TW",
		"zh-Hant;q=0.9":                      "zh-TW",
		"fr-CH, fr;q=0.9, en;q=0.8, *;q=0.5": "en",
		"ja":                                 "zh-CN",
		"zh-Hans-CN":                         "zh-CN",
	}
	for header, want := range cases {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		request.Header.Set("Accept-Language", header)
		if got := requestLocale(request); got != want {
			t.Fatalf("Accept-Language %q locale = %q, want %q", header, got, want)
		}
	}
}

func TestServer_ExtensionSessionSnapshot(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("X-Session-ID", "lspg-abc12")
	session := extensionSession(request, auth.Identity{Legacy: true})
	if session.Anonymous != true || session.KeyID != "anonymous" || session.ID != "" {
		t.Fatalf("session = %+v", session)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("X-Session-ID", "lspg-abc12")
	request.Header.Set("Authorization", "Bearer sk-test")
	// Before CreateRequestContextMiddleware runs there is no metadata, so the
	// session falls back to the identity fields.
	session = extensionSession(request, auth.Identity{})
	if !session.Anonymous || session.KeyID != "anonymous" || session.ID != "" {
		t.Fatalf("no-metadata session = %+v", session)
	}

	data := swaputil.ReqContextData{Model: "m", ModelID: "m"}
	data.Metadata = map[string]string{"key_id": "key-1", "session_id": "lspg-abc12"}
	request = request.WithContext(swaputil.SetContext(request.Context(), data))
	session = extensionSession(request, auth.Identity{ID: "key-1"})
	if session.Anonymous || session.KeyID != "key-1" || session.ID != "lspg-abc12" {
		t.Fatalf("session = %+v", session)
	}
}

func TestServer_ExtensionModelSnapshotFilters(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"a": {Name: "Alpha"}, "b": {Disabled: true}, "c": {},
	}}
	all := extensionModelSnapshot(cfg, auth.Identity{})
	if len(all) != 2 || all[0].ID != "a" || all[0].Name != "Alpha" || all[1].ID != "c" {
		t.Fatalf("snapshot = %+v", all)
	}
	scoped := extensionModelSnapshot(cfg, auth.Identity{Models: []string{"c"}})
	if len(scoped) != 1 || scoped[0].ID != "c" {
		t.Fatalf("scoped snapshot = %+v", scoped)
	}
}
