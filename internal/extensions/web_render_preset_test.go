package extensions

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// webRenderFixture is the reader service's JSON envelope with the parts this tool
// reads named the way the API documents them.
const webRenderFixture = `{
  "code": 200,
  "status": 20000,
  "data": {
    "title": "Understanding Widgets",
    "description": "A page about widgets",
    "url": "https://example.com/widgets",
    "content": "# Understanding widgets\n\nThe first widget was built in 1846; see the [specification](https://example.com/docs/spec).\n\n- Spin the crank\n- Watch the gears turn\n"
  }
}`

func webRenderPreset(t *testing.T, id string, config map[string]any) *Compiled {
	t.Helper()
	compiled := presetBundle(t, id)
	if compiled == nil {
		t.Fatalf("%s is not in the catalog", id)
	}
	compiled.Manifest.Config = config
	return compiled
}

func TestPresets_WebRenderAsksTheServiceAndFormatsTheAnswer(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", nil)
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, webRenderFixture }}

	content, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "example.com/widgets"}, host)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"Source: https://example.com/widgets",
		"Title: Understanding Widgets",
		"# Understanding widgets",
		"[specification](https://example.com/docs/spec)",
		"rendered page",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("result is missing %q:\n%s", want, content)
		}
	}
	// The request has to be the documented one: the target is percent-encoded
	// into the reader's path, the JSON envelope is requested, and the API key
	// travels as a bearer token rather than a query parameter.
	if len(host.calls) != 1 {
		t.Fatalf("the tool made %d requests", len(host.calls))
	}
	call := host.calls[0]
	if call.Method != "GET" {
		t.Fatalf("method = %q", call.Method)
	}
	if want := "https://r.jina.ai/https://example.com/widgets"; call.URL != want {
		t.Fatalf("url = %q, want %q", call.URL, want)
	}
	if host.calls[0].Headers["Accept"] != "application/json" {
		t.Fatalf("Accept = %v", host.calls[0].Headers["Accept"])
	}
	if _, present := call.Headers["Authorization"]; present {
		t.Fatalf("an API key was sent when none is configured: %v", call.Headers)
	}
}

func TestPresets_WebRenderPassesSelectorsAndKey(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", map[string]any{
		"apiKey":          "jina-test-key",
		"waitForSelector": "#results",
		"targetSelector":  "main",
		"noCache":         true,
		"timeoutSeconds":  45,
		"includeLinks":    false,
	})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, webRenderFixture }}
	if _, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.com/app"}, host); err != nil {
		t.Fatalf("render: %v", err)
	}
	headers := host.calls[0].Headers
	for name, want := range map[string]string{
		"Authorization":       "Bearer jina-test-key",
		"X-Wait-For-Selector": "#results",
		"X-Target-Selector":   "main",
		"X-No-Cache":          "true",
		"X-Timeout":           "45",
	} {
		got, _ := headers[name].(string)
		if got != want {
			t.Fatalf("header %s = %q, want %q (headers: %v)", name, got, want, headers)
		}
	}
	// The link-keeping setting turns into a text request when the caller did not
	// ask for a format.
	if got, _ := headers["X-Return-Format"].(string); got != "text" {
		t.Fatalf("X-Return-Format = %q, want text when includeLinks is off", got)
	}
}

// TestPresets_WebRenderCustomEndpoint pins the self-hosted path: the template in
// the settings is used verbatim, with only {url} replaced.
func TestPresets_WebRenderCustomEndpoint(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", map[string]any{
		"service":  "custom",
		"endpoint": "https://render.example.com/api/render?target={url}&format=md",
	})
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, "# Rendered by the self-hosted service" }}
	content, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.org/page?a=1&b=2"}, host)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "https://render.example.com/api/render?target=" + url.QueryEscape("https://example.org/page?a=1&b=2") + "&format=md"
	if host.calls[0].URL != want {
		t.Fatalf("url = %q, want %q", host.calls[0].URL, want)
	}
	// A plain Markdown body (no JSON envelope) is accepted as-is.
	if !strings.Contains(content, "Rendered by the self-hosted service") {
		t.Fatalf("a plain body was not used:\n%s", content)
	}

	// A path-style template uses {urlRaw}, so a query string in the target is
	// part of the path rather than the renderer's own parameters.
	pathStyle := webRenderPreset(t, "web-render", map[string]any{
		"service":  "custom",
		"endpoint": "https://render.example.com/{urlRaw}",
	})
	if _, err := runPresetTool(t, pathStyle, "web_render", map[string]any{"url": "https://example.org/page?a=1&b=2"}, host); err != nil {
		t.Fatalf("path-style render: %v", err)
	}
	if got := host.calls[len(host.calls)-1].URL; got != "https://render.example.com/https://example.org/page?a=1&b=2" {
		t.Fatalf("path-style url = %q", got)
	}
	// A template with neither placeholder cannot address a target.
	neither := webRenderPreset(t, "web-render", map[string]any{"service": "custom", "endpoint": "https://render.example.com/all"})
	if _, err := runPresetTool(t, neither, "web_render", map[string]any{"url": "https://example.com"}, host); err == nil {
		t.Fatal("a template with no placeholder was accepted")
	} else if !strings.Contains(err.Error(), "urlRaw") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestPresets_WebRenderRejectsUnusableInput(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", nil)
	host := &scriptedHost{respond: func(scriptedCall) (int, string) { return 200, webRenderFixture }}
	for _, entry := range []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"no url", map[string]any{}, "url is required"},
		{"file scheme", map[string]any{"url": "file:///etc/passwd"}, "only http and https"},
		{"javascript scheme", map[string]any{"url": "javascript:alert(1)"}, "only http and https"},
		{"credentials", map[string]any{"url": "https://user:secret@example.com/private"}, "remove the credentials"},
	} {
		_, err := runPresetTool(t, compiled, "web_render", entry.arguments, host)
		if err == nil {
			t.Fatalf("%s: the tool accepted input it should have rejected", entry.name)
		}
		if !strings.Contains(err.Error(), entry.want) {
			t.Fatalf("%s: error %q does not mention %q", entry.name, err.Error(), entry.want)
		}
	}
	if len(host.calls) != 0 {
		t.Fatalf("a rejected call still reached the network: %+v", host.calls)
	}
	// A custom service without an endpoint has to say so, rather than building a
	// request to nowhere.
	broken := webRenderPreset(t, "web-render", map[string]any{"service": "custom"})
	if _, err := runPresetTool(t, broken, "web_render", map[string]any{"url": "https://example.com"}, host); err == nil {
		t.Fatal("an empty custom endpoint was accepted")
	} else if !strings.Contains(err.Error(), "custom endpoint") {
		t.Fatalf("error = %q", err.Error())
	}
}

// TestPresets_WebRenderReportsServiceFailures pins that an HTTP failure comes
// back as a readable result naming what to do, not as a bare status.
func TestPresets_WebRenderReportsServiceFailures(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", nil)
	for _, entry := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"code":401,"readableMessage":"AuthenticationRequiredError: authenticate"}`, "API key"},
		{429, `{"message":"rate limited"}`, "rate limiting"},
		{404, "not found", "could not find that page"},
	} {
		host := &scriptedHost{respond: func(scriptedCall) (int, string) { return entry.status, entry.body }}
		content, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.com/x"}, host)
		if err != nil {
			t.Fatalf("status %d: %v", entry.status, err)
		}
		if !strings.Contains(content, entry.want) {
			t.Fatalf("status %d answer is missing %q:\n%s", entry.status, entry.want, content)
		}
	}
	// An error inside a 200 envelope is still an error.
	host := &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 200, `{"code":403,"name":"ForbiddenError","message":"blocked"}`
	}}
	if _, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.com/x"}, host); err == nil {
		t.Fatal("an error envelope was accepted as content")
	} else if !strings.Contains(err.Error(), "refused the request") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestPresets_WebRenderTruncatesAndReportsEmptyPages(t *testing.T) {
	compiled := webRenderPreset(t, "web-render", nil)
	long := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 300)
	host := &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 200, `{"code":200,"data":{"title":"Long","url":"https://example.com/long","content":` + quoteJSON(long) + `}}`
	}}
	content, err := runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.com/long", "maxChars": 1000}, host)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(content, "characters omitted") {
		t.Fatalf("truncation was not reported:\n%s", content[:minInt(len(content), 400)])
	}
	if len(content) > 2600 {
		t.Fatalf("maxChars was not applied: %d characters", len(content))
	}
	// A page with no text at all must say so rather than return nothing.
	host = &scriptedHost{respond: func(scriptedCall) (int, string) {
		return 200, `{"code":200,"data":{"title":"","url":"https://example.com/blank","content":""}}`
	}}
	content, err = runPresetTool(t, compiled, "web_render", map[string]any{"url": "https://example.com/blank"}, host)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if !strings.Contains(content, "no readable text") {
		t.Fatalf("an empty page was not explained:\n%s", content)
	}
}

func quoteJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
