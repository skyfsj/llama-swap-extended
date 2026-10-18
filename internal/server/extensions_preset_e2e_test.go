package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// TestAPIExtensions_NewPresetsInstallAndRun is the store's end-to-end path for
// the media and utilities presets: the catalogue offers them, installing one
// writes a working extension, and the installed tool answers through the test
// endpoint - which is where the daemon hosts the image operation the preset asks
// for. The per-preset unit tests check what each tool says; this checks the
// wiring an operator actually goes through.
func TestAPIExtensions_NewPresetsInstallAndRun(t *testing.T) {
	dir := t.TempDir()
	s := extensionAPITestServer(t, dir)

	listed := extensionAPIRequest(t, s, http.MethodGet, "/api/extensions/presets", "", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list presets status=%d %s", listed.Code, listed.Body.String())
	}
	var catalog struct {
		Data []struct {
			ID       string   `json:"id"`
			Category string   `json:"category"`
			Tools    []string `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"image-tools":    {"image_crop", "image_resize", "image_analyze"},
		"unit-convert":   {"unit_convert"},
		"math-tools":     {"calculator", "symbolic_math"},
		"web-render":     {"web_render"},
		"datetime-tools": {"get_datetime", "timezone_convert"},
	}
	found := map[string]bool{}
	categories := map[string]bool{}
	for _, entry := range catalog.Data {
		categories[entry.Category] = true
		tools, known := want[entry.ID]
		if !known {
			continue
		}
		found[entry.ID] = true
		// The catalogue sorts tool names, so compare as sets.
		listed := append([]string(nil), entry.Tools...)
		sort.Strings(listed)
		expectedTools := append([]string(nil), tools...)
		sort.Strings(expectedTools)
		if strings.Join(listed, ",") != strings.Join(expectedTools, ",") {
			t.Fatalf("%s lists tools %v, want %v", entry.ID, entry.Tools, tools)
		}
	}
	for id := range want {
		if !found[id] {
			t.Fatalf("the store does not offer %s", id)
		}
	}
	// A category the console has no label for renders as a raw key, so both new
	// groups have to be present in the catalogue the UI reads.
	for _, category := range []string{"media", "utilities"} {
		if !categories[category] {
			t.Fatalf("no preset uses the %q category", category)
		}
	}

	// Install the one that needs the daemon's help, then run a tool through the
	// test endpoint.
	created := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/presets/image-tools/install", `{}`, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("install status=%d %s", created.Code, created.Body.String())
	}
	// The injected tool definitions reach the model through the ordinary
	// onRequest pipeline, which is what makes the installed tool callable.
	tested := extensionAPIRequest(t, s, http.MethodPost, "/api/extensions/image-tools/test",
		`{"context":{"endpoint":"chat.completions"},"request":{"model":"m","messages":[]}}`, nil)
	if tested.Code != http.StatusOK {
		t.Fatalf("test endpoint status=%d %s", tested.Code, tested.Body.String())
	}
	var injected struct {
		InjectedTools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"injectedTools"`
	}
	if err := json.Unmarshal(tested.Body.Bytes(), &injected); err != nil {
		t.Fatalf("test body: %v (%s)", err, tested.Body.String())
	}
	names := make([]string, 0, len(injected.InjectedTools))
	for _, tool := range injected.InjectedTools {
		names = append(names, tool.Function.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "image_analyze,image_crop,image_resize" {
		t.Fatalf("the installed preset injected %v", names)
	}

	// Run one tool the way the live pipeline does: through the compiled
	// definition the manager holds, with the daemon answering the host call.
	item, ok := s.extensions.Compiled("image-tools")
	if !ok {
		t.Fatal("the installed extension did not compile")
	}
	payload, err := json.Marshal(map[string]any{
		"name":      "image_analyze",
		"arguments": map[string]any{"image": testPNGPayload(t, 60, 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The daemon installs the host-call executor for the request, which is how
	// the live pipeline reaches images.apply; the test does the same.
	request := httptest.NewRequest(http.MethodPost, "/api/models/m/inference", nil)
	ctx := extensions.WithHostCaller(request.Context(), newExtensionHost(s, request))
	out, _, err := item.Invoke(ctx, "onToolCall", extensions.Context{Endpoint: "chat.completions"}, payload)
	if err != nil {
		t.Fatalf("the installed tool failed: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("tool output %s: %v", string(out), err)
	}
	if !strings.Contains(result.Content, "size: 60x40") {
		t.Fatalf("the installed tool answered %q", result.Content)
	}
}

// testPNGPayload renders a small deterministic PNG as base64.
func testPNGPayload(t *testing.T, width, height int) string {
	t.Helper()
	fixture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			fixture.SetRGBA(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 6), B: 90, A: 255})
		}
	}
	var buffer bytes.Buffer
	encoder := base64.NewEncoder(base64.StdEncoding, &buffer)
	if err := png.Encode(encoder, fixture); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}
