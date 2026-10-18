package extensions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestManifest_NetworkHostPatterns(t *testing.T) {
	accepted := []string{"example.com", "*.pinecone.io", "api.example.co.uk", "*", "127.0.0.1:8080"}
	for _, pattern := range accepted {
		manifest := Manifest{ID: "x", Permissions: Permissions{NetworkHosts: []string{pattern}}}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("pattern %q was rejected: %v", pattern, err)
		}
	}
	// A wildcard belongs at the front of a domain or nowhere: "*foo" and "a*b"
	// are typos that would quietly widen a permission the author wrote down.
	rejected := []string{"", "*foo", "a*b", "foo*", "*.*", "exa mple.com", "http://example.com"}
	for _, pattern := range rejected {
		manifest := Manifest{ID: "x", Permissions: Permissions{NetworkHosts: []string{pattern}}}
		if err := manifest.Validate(); err == nil {
			t.Fatalf("pattern %q was accepted", pattern)
		}
	}
}

// TestManifest_NetworkDefaultStopsAtTheLocalNetwork is the guarantee the
// default grant rests on: an empty list (or "*") covers public hosts, and the
// address check still refuses anything local or private. Literal addresses are
// used so the test never depends on a resolver.
func TestManifest_NetworkDefaultStopsAtTheLocalNetwork(t *testing.T) {
	for _, hosts := range [][]string{nil, {}, {"*"}} {
		for _, target := range []string{
			"http://127.0.0.1:8080/admin",
			"http://192.168.1.10/",
			"http://[::1]:9000/",
			"http://169.254.169.254/latest/meta-data/",
		} {
			_, err := FetchAllowed(hosts, target, nil)
			if err == nil {
				t.Fatalf("%s was fetchable with hosts %v", target, hosts)
			}
			// The refusal has to come from the address check, not from the
			// list: that is what makes the default grant safe.
			if !strings.Contains(err.Error(), "not a public address") {
				t.Fatalf("%s with hosts %v: error = %v, want the public-address refusal", target, hosts, err)
			}
		}
	}

	// A listed host narrows the grant, and the refusal happens before anything
	// is resolved.
	_, err := FetchAllowed([]string{"api.example.com"}, "https://other.example.org/", nil)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("list refusal = %v", err)
	}
	if _, err := FetchAllowed([]string{"api.example.com"}, "https://api.example.com/v1", nil); err == nil {
		t.Fatal("a listed host was refused")
	}

	// Neither form is a licence to skip the URL rules.
	for _, target := range []string{"file:///etc/passwd", "https://user:pass@example.com/", "not a url"} {
		if _, err := FetchAllowed(nil, target, nil); err == nil {
			t.Fatalf("%s was accepted as a fetchable URL", target)
		}
	}
}

func TestFetch_TimeoutOption(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
		want    time.Duration
	}{
		{"absent", nil, defaultFetchTimeout},
		{"explicit", map[string]any{"timeoutMs": float64(20000)}, 20 * time.Second},
		{"json number", map[string]any{"timeoutMs": json.Number("30000")}, 30 * time.Second},
		{"below the floor", map[string]any{"timeoutMs": float64(50)}, minFetchTimeout},
		{"above the ceiling", map[string]any{"timeoutMs": float64(600000)}, maxFetchTimeout},
		{"nonsense", map[string]any{"timeoutMs": "soon"}, defaultFetchTimeout},
		{"negative", map[string]any{"timeoutMs": float64(-1)}, defaultFetchTimeout},
	}
	for _, entry := range cases {
		if got := fetchTimeout(entry.options); got != entry.want {
			t.Fatalf("%s: timeout = %v, want %v", entry.name, got, entry.want)
		}
	}
}

func TestFetch_HeadersAreLowercased(t *testing.T) {
	rendered := HeadersToMap(map[string][]string{
		"Content-Type": {"text/html; charset=utf-8"},
		"Set-Cookie":   {"a=1", "b=2"},
	})
	if rendered["content-type"] != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %v", rendered["content-type"])
	}
	if rendered["set-cookie"] != "a=1, b=2" {
		t.Fatalf("repeated headers = %v", rendered["set-cookie"])
	}
	if _, present := rendered["Content-Type"]; present {
		t.Fatal("a header kept its original casing")
	}
}

// TestFetch_ResponseValueIsWhatScriptsRead pins the shape ctx.http.fetch
// resolves to: a script reads response.headers and response.url, so a field
// dropped here is a silently broken tool.
func TestFetch_ResponseValueIsWhatScriptsRead(t *testing.T) {
	value := fetchResponseValue(FetchResult{
		Body:     []byte(`{"ok":true}`),
		Status:   201,
		Header:   map[string][]string{"Content-Type": {"application/json"}},
		FinalURL: "https://example.com/final",
	})
	if value["status"] != 201 || value["ok"] != true {
		t.Fatalf("status/ok = %v/%v", value["status"], value["ok"])
	}
	if value["url"] != "https://example.com/final" {
		t.Fatalf("url = %v", value["url"])
	}
	headers, ok := value["headers"].(map[string]any)
	if !ok || headers["content-type"] != "application/json" {
		t.Fatalf("headers = %v", value["headers"])
	}
	text, ok := value["text"].(func() string)
	if !ok || text() != `{"ok":true}` {
		t.Fatalf("text() = %v", value["text"])
	}
	parse, ok := value["json"].(func() any)
	if !ok {
		t.Fatalf("json() = %v", value["json"])
	}
	parsed, ok := parse().(map[string]any)
	if !ok || parsed["ok"] != true {
		t.Fatalf("json() = %v", parse())
	}
	// A failure status must not look like success to a script.
	failed := fetchResponseValue(FetchResult{Status: 404})
	if failed["ok"] != false {
		t.Fatalf("404 reported ok=%v", failed["ok"])
	}
}
