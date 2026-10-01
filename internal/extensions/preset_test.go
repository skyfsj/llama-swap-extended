package extensions

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPresets_MissingCredentialIsReportedClearly(t *testing.T) {
	// An installed preset has an empty credential until the user fills it in.
	// The script must say so instead of sending a request that can only 401.
	compiled := presetBundle(t, "web-search-bing")
	if compiled == nil {
		t.Skip("web-search-bing is not in the catalog")
	}
	_, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, json.RawMessage(`{"name":"web_search","arguments":{"query":"llama-swap"}}`))
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestPresets_OpenAIWebSearchInjection(t *testing.T) {
	compiled := presetBundle(t, "openai-web-search")
	if compiled == nil {
		t.Skip("openai-web-search is not in the catalog")
	}
	out, _, err := compiled.Invoke(context.Background(), "onRequest", Context{Endpoint: "chat.completions"}, json.RawMessage(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatalf("chat.completions injection: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("chat tools = %+v", chat["tools"])
	}
	if entry, _ := tools[0].(map[string]any); entry["type"] != "web_search" {
		t.Fatalf("chat tool entry = %+v", tools[0])
	}

	out, _, err = compiled.Invoke(context.Background(), "onRequest", Context{Endpoint: "responses"}, json.RawMessage(`{"model":"gpt-4o","tools":[{"type":"web_search"}]}`))
	if err != nil {
		t.Fatalf("responses injection: %v", err)
	}
	var responses map[string]any
	if err := json.Unmarshal(out, &responses); err != nil {
		t.Fatal(err)
	}
	existing, _ := responses["tools"].([]any)
	if len(existing) != 1 {
		t.Fatalf("a client's own web_search tool was duplicated: %+v", responses["tools"])
	}
}

// presetBundle bundles one preset and inspects it, which is how a preset is
// exercised without installing it anywhere.
func presetBundle(t *testing.T, id string) *Compiled {
	t.Helper()
	for _, preset := range Presets() {
		if preset.ID != id {
			continue
		}
		bundle, err := bundleTree(preset.Files)
		if err != nil {
			t.Fatalf("bundle %s: %v", id, err)
		}
		compiled := &Compiled{Bundle: bundle, Manifest: preset.Manifest}
		if err := compiled.Inspect(context.Background()); err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		}
		return compiled
	}
	return nil
}

func TestPresets_CatalogLoads(t *testing.T) {
	presets := Presets()
	if len(presets) < 4 {
		t.Fatalf("catalog only has %d presets: %v", len(presets), presetProblems())
	}
	for _, preset := range presets {
		if preset.ID == "" || preset.Name == "" || preset.Description == "" {
			t.Fatalf("preset %s is incomplete: %+v", preset.ID, preset)
		}
		if preset.Category == "" || preset.Category == "other" {
			t.Fatalf("preset %s has no category", preset.ID)
		}
		if _, ok := preset.Files[EntryFile]; !ok {
			t.Fatalf("preset %s has no %s", preset.ID, EntryFile)
		}
	}
	if problems := presetProblems(); len(problems) != 0 {
		t.Fatalf("presets were dropped from the catalog: %v", problems)
	}
}

func TestPresets_Install(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	preset, ok := PresetByID("web-search-bing")
	if !ok {
		t.Skip("bing preset is not in the catalog")
	}
	installed, err := m.InstallPreset(context.Background(), preset, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Manifest.ID != preset.ID || installed.Status != "ready" {
		t.Fatalf("installed = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(root, preset.ID, EntryFile)); err != nil {
		t.Fatalf("entrypoint missing: %v", err)
	}
	if _, err := m.InstallPreset(context.Background(), preset, ""); err == nil {
		t.Fatal("installing twice was allowed")
	}
	// Installing under another id is how a user runs two search backends.
	second, err := m.InstallPreset(context.Background(), preset, "bing-backup")
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Manifest.ID != "bing-backup" || len(m.List()) != 2 {
		t.Fatalf("second install = %+v list=%d", second, len(m.List()))
	}
}

func TestPresets_DeclareNoCredentials(t *testing.T) {
	// A preset ships configuration knobs, never a key: a literal one would be
	// the same for every user who installs it.
	assignments := regexp.MustCompile(`(?m)^\s*(?:const|let|var)\s+[A-Za-z0-9_]*(?:key|token|secret|password)\w*\s*=\s*["'][^"']{8,}["']`)
	defaults := regexp.MustCompile(`default:\s*["'][^"']{8,}["']`)
	for _, preset := range Presets() {
		for name, data := range preset.Files {
			if assignments.MatchString(data) {
				t.Fatalf("preset %s/%s assigns a credential literal", preset.ID, name)
			}
			if defaults.MatchString(data) {
				t.Fatalf("preset %s/%s carries a credential-looking default", preset.ID, name)
			}
		}
		for _, field := range preset.Settings {
			if field.Secret && field.Default != nil {
				t.Fatalf("preset %s setting %s has a default secret", preset.ID, field.Key)
			}
		}
	}
}

func TestPresets_DeclareOnlyPublicHosts(t *testing.T) {
	for _, preset := range Presets() {
		for _, host := range preset.Hosts {
			// A leading subdomain wildcard is the only wildcard shape allowed,
			// because a bare one would hand the extension every host.
			if host == "" {
				t.Fatalf("preset %s declares an empty host", preset.ID)
			}
			if strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
				t.Fatalf("preset %s declares wildcard host %q", preset.ID, host)
			}
			if strings.ContainsAny(host, "?[]") {
				t.Fatalf("preset %s declares host %q", preset.ID, host)
			}
			if _, err := netip.ParseAddr(host); err == nil {
				t.Fatalf("preset %s declares the address %q instead of a hostname", preset.ID, host)
			}
			if err := validateHostPattern(host); err != nil {
				t.Fatalf("preset %s declares host %q: %v", preset.ID, host, err)
			}
		}
	}
}

func TestCheckPublicHost(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "localhost", "10.1.2.3", "192.168.1.10", "172.16.0.5",
		"169.254.169.254", "0.0.0.0", "::1", "fc00::1", "fe80::1", "100.64.0.1",
		"240.0.0.1", "255.255.255.255", "metadata.google.internal",
	}
	for _, host := range blocked {
		if err := checkPublicHost(host); err == nil {
			t.Fatalf("host %s was allowed", host)
		}
	}
	// A public address must pass. The resolver is not consulted for literals,
	// so this assertion does not depend on the network.
	if err := checkPublicHost("8.8.8.8"); err != nil {
		t.Fatalf("public literal rejected: %v", err)
	}
	if err := checkPublicHost("2606:4700:4700::1111"); err != nil {
		t.Fatalf("public IPv6 literal rejected: %v", err)
	}
}

func TestPresets_InstallRefusesInvalidTarget(t *testing.T) {
	presets := Presets()
	if len(presets) == 0 {
		t.Skip("no presets")
	}
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallPreset(context.Background(), presets[0], "bad id"); err == nil {
		t.Fatal("invalid target id was accepted")
	}
}

func TestPresets_SettingsAreRenderable(t *testing.T) {
	raw, err := json.Marshal(Presets())
	if err != nil {
		t.Fatal(err)
	}
	// The UI renders the store from this payload; settings must survive the
	// round trip with the fields the form needs.
	var decoded []struct {
		ID       string `json:"id"`
		Settings []struct {
			Key       string   `json:"key"`
			Component string   `json:"component"`
			Label     string   `json:"label"`
			Options   []string `json:"options"`
		} `json:"settings"`
		Hosts []string `json:"hosts"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, preset := range decoded {
		for _, field := range preset.Settings {
			if field.Key == "" || field.Component == "" || field.Label == "" {
				t.Fatalf("preset %s has an unusable setting %+v", preset.ID, field)
			}
		}
	}
}
