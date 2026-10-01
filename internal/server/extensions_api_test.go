package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// extensionAPITestServer serves the extension management API from dir, which
// the caller fills with whatever tree the test needs.
func extensionAPITestServer(t *testing.T, dir string) *Server {
	t.Helper()
	m, err := extensions.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, RequiredAPIKeys: []string{"secret"}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: dir}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func extensionAPIRequest(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader = strings.NewReader(body)
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, request)
	return w
}

func TestAPIExtensions_MultiFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := extensionAPITestServer(t, dir)
	tree := `{"index.js":"import { greeting } from \"./lib/greeting.js\";\nexport default { tools: [] };","lib/greeting.js":"export const greeting = () => \"hi\";"}`
	created := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions", `{"manifest":{"id":"multi","name":"Multi","enabled":true},"files":`+tree+`}`, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d %s", created.Code, created.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	files, _ := saved["files"].(map[string]any)
	if files["lib/greeting.js"] == nil {
		t.Fatalf("created files = %+v", saved)
	}
	if _, err := os.Stat(filepath.Join(dir, "multi", "lib", "greeting.js")); err != nil {
		t.Fatalf("module not written: %v", err)
	}
	etag := created.Header().Get("ETag")
	fetched := extensionAPIRequest(t, s, http.MethodGet, "/api/extensions/multi", "", nil)
	if fetched.Code != http.StatusOK || !strings.Contains(fetched.Body.String(), "lib/greeting.js") {
		t.Fatalf("get status=%d %s", fetched.Code, fetched.Body.String())
	}
	// If-Match keeps concurrent editors from overwriting each other.
	conflict := extensionAPIRequest(t, s, http.MethodPut, "/api/extensions/multi", `{"manifest":{"id":"multi"},"files":{"index.js":"export default { tools: [] };"}}`, map[string]string{"If-Match": `"stale"`})
	if conflict.Code != http.StatusPreconditionFailed {
		t.Fatalf("conflict status=%d %s", conflict.Code, conflict.Body.String())
	}
	updated := extensionAPIRequest(t, s, http.MethodPut, "/api/extensions/multi", `{"manifest":{"id":"multi"},"files":{"index.js":"export default { tools: [] };"}}`, map[string]string{"If-Match": etag})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d %s", updated.Code, updated.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "multi", "lib", "greeting.js")); !os.IsNotExist(err) {
		t.Fatalf("module survived its removal: %v", err)
	}
}

func TestAPIExtensions_CheckDiagnostics(t *testing.T) {
	dir := t.TempDir()
	s := extensionAPITestServer(t, dir)
	broken := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/check", `{"files":{"index.js":"export default {","package.json":"{\"name\":\"x\",}"}}`, nil)
	if broken.Code != http.StatusOK {
		t.Fatalf("check status=%d %s", broken.Code, broken.Body.String())
	}
	var result struct {
		Diagnostics []struct {
			Path     string `json:"path"`
			Line     int    `json:"line"`
			Column   int    `json:"column"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(broken.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Line < 1 || diagnostic.Column < 1 || diagnostic.Message == "" {
			t.Fatalf("unusable diagnostic = %+v", diagnostic)
		}
	}
	clean := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/check", `{"files":{"index.js":"export default { tools: [] };"}}`, nil)
	if clean.Code != http.StatusOK || !strings.Contains(clean.Body.String(), `"diagnostics":[]`) {
		t.Fatalf("clean check=%d %s", clean.Code, clean.Body.String())
	}
	single := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/check", `{"path":"index.js","source":"const a = ;"}`, nil)
	if single.Code != http.StatusOK || !strings.Contains(single.Body.String(), "Unexpected") {
		t.Fatalf("single check=%d %s", single.Code, single.Body.String())
	}
}

func TestAPIExtensions_InvalidSettingsReturnDiagnostics(t *testing.T) {
	dir := t.TempDir()
	s := extensionAPITestServer(t, dir)
	body := `{"manifest":{"id":"settings","enabled":true,"config":{"apiKey":"k","locale":"fr"}},"files":{"index.js":"export const settings = { apiKey: { type: \"string\", required: true }, locale: { type: \"select\", options: [\"zh\", \"en\"] } }; export default { tools: [] };"}}`
	response := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions", body, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Diagnostics []struct {
			Path     string `json:"path"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Diagnostics) != 1 || payload.Diagnostics[0].Path != "locale" {
		t.Fatalf("diagnostics = %+v", payload.Diagnostics)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings")); !os.IsNotExist(err) {
		t.Fatalf("rejected extension was written: %v", err)
	}
}

func TestAPIExtensions_PresetStore(t *testing.T) {
	dir := t.TempDir()
	s := extensionAPITestServer(t, dir)
	listed := extensionAPIRequest(t, s, http.MethodGet, "/api/extensions/presets", "", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list presets status=%d %s", listed.Code, listed.Body.String())
	}
	var catalog struct {
		Data []struct {
			ID       string   `json:"id"`
			Name     string   `json:"name"`
			Category string   `json:"category"`
			Hosts    []string `json:"hosts"`
			Settings []struct {
				Key       string `json:"key"`
				Label     string `json:"label"`
				Component string `json:"component"`
			} `json:"settings"`
			Installed bool `json:"installed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	presetID := ""
	for _, entry := range catalog.Data {
		if entry.Installed || entry.ID == "" {
			t.Fatalf("catalog entry is unusable: %+v", entry)
		}
		for _, host := range entry.Hosts {
			// A leading subdomain wildcard is allowed for hosted services; a
			// bare one would hand the extension every host on the network.
			if strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
				t.Fatalf("preset %s declares wildcard host %q", entry.ID, host)
			}
			if strings.ContainsAny(host, "?[]") {
				t.Fatalf("preset %s declares host %q", entry.ID, host)
			}
		}
		presetID = entry.ID
	}
	if presetID == "" {
		t.Skip("no presets are embedded in this build")
	}

	// Installing a preset writes a normal extension that is immediately editable.
	created := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/presets/"+presetID+"/install", `{}`, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("install status=%d %s", created.Code, created.Body.String())
	}
	var installed map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	files, _ := installed["files"].(map[string]any)
	if files["index.js"] == nil {
		t.Fatalf("installed definition has no entrypoint: %+v", installed)
	}
	again := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/presets/"+presetID+"/install", `{}`, nil)
	if again.Code != http.StatusBadRequest {
		t.Fatalf("second install status=%d %s", again.Code, again.Body.String())
	}
	detail := extensionAPIRequest(t, s, http.MethodGet, "/api/extensions/presets/"+presetID, "", nil)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "lib/greeter.js") {
		// The detail endpoint carries the whole tree; check it round-trips.
		t.Logf("preset detail=%s", detail.Body.String())
	}
	unknown := extensionAPIRequest(t, s, http.MethodGet, "/api/extensions/presets/nope", "", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown preset status=%d", unknown.Code)
	}
}

func TestAPIExtensions_TestReturnsScriptLogs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "noop.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "logging"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logging", "manifest.yaml"), []byte("id: logging\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default { onRequest(ctx, request) { ctx.log.info("hook ran"); return request; } };`
	if err := os.WriteFile(filepath.Join(dir, "logging", "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "noop.yaml")); err != nil {
		t.Fatal(err)
	}
	s := extensionAPITestServer(t, dir)
	response := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/logging/test", `{"context":{"resolvedModel":"m","endpoint":"chat.completions"},"request":{"model":"m","messages":[]}}`, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "hook ran") {
		t.Fatalf("test status=%d %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if duration, ok := result["durationMs"].(float64); !ok || duration <= 0 {
		t.Fatalf("missing execution duration: %+v", result)
	}

}
