package extensions

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// allowLoopback replaces the host guard for the duration of a test. The guard is
// exactly what stops FetchAllowed from reaching 127.0.0.1, so a test that serves
// from a local server has to take it out; it has its own tests in
// TestManifest_NetworkDefaultStopsAtTheLocalNetwork and TestCheckPublicHost.
func allowLoopback(t *testing.T) {
	t.Helper()
	previous := publicHostCheck
	publicHostCheck = func(string) error { return nil }
	t.Cleanup(func() { publicHostCheck = previous })
}

// TestFetch_EmptyHostListIsTheDefaultGrant pins the posture the permission model
// rests on: an extension that names no hosts may still fetch, and what bounds it
// is the public-address guard. That is why the loopback server here needs the
// guard swapped out and nothing else.
func TestFetch_EmptyHostListIsTheDefaultGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	}))
	defer server.Close()

	// With the real guard the same call is refused, and the refusal comes from
	// the address check rather than from the (empty) host list.
	if _, err := FetchAllowed(nil, server.URL, nil); err == nil {
		t.Fatal("a loopback address was reachable through the real guard")
	} else if !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("error = %v, want the public-address refusal", err)
	}

	allowLoopback(t)
	result, err := FetchAllowed(nil, server.URL, nil)
	if err != nil {
		t.Fatalf("the default grant refused a public host: %v", err)
	}
	if string(result.Body) != "reached" {
		t.Fatalf("body = %q", result.Body)
	}
}

// guardMarker is what the replaced guard answers with, so the test can tell its
// refusal apart from the ones the fetcher raises itself.
const guardMarker = "the guard was consulted for this hop"

// TestFetch_RedirectHopsUseTheGuardSeam pins that every redirect hop is
// re-checked through the same guard as the first request. A hop is the part of
// a fetch a page chooses for itself, so a guard that only covers the first
// request is worth very little.
func TestFetch_RedirectHopsUseTheGuardSeam(t *testing.T) {
	previous := publicHostCheck
	publicHostCheck = func(host string) error {
		if host == "internal.example" {
			return errors.New(guardMarker)
		}
		return nil
	}
	t.Cleanup(func() { publicHostCheck = previous })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.example/next", http.StatusFound)
	}))
	defer server.Close()

	_, err := FetchAllowed([]string{"*"}, server.URL+"/start", nil)
	if err == nil || !strings.Contains(err.Error(), guardMarker) {
		t.Fatalf("redirect hop error = %v, want the guard's refusal", err)
	}
}

// TestPresets_WebFetchRoundTrip runs the tool against a real HTTP response and
// checks what the extension does with it: the browser headers it sends, that it
// follows a redirect and reports the address it landed on, that a gzipped body
// arrives decoded, and that the page is converted with its furniture removed.
//
// It drives the Node executor rather than the default goja worker because the
// host call then runs in this process, which is what lets the request reach a
// loopback server. Both runtimes answer ctx.http.fetch the same way; the goja
// side's response object is covered by TestFetch_ResponseValueIsWhatScriptsRead.
func TestPresets_WebFetchRoundTrip(t *testing.T) {
	nodePath, err := lookPathCached()
	if err != nil {
		t.Skip("node is not available")
	}
	script := executorScriptPath()
	if script == "" {
		t.Skip("the executor script is not available")
	}

	page := `<!doctype html><html lang="en"><head><title>Fetched page | Site</title></head><body>
<nav><a href="/x">Nav link</a></nav>
<main><article><h1>Fetched page</h1><p>Body text from the server, long enough to look like an article rather than a nav block, so the candidate scoring has something to work with.</p></article></main>
<footer>Footer text</footer></body></html>`

	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/page", http.StatusFound)
			return
		}
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			// The response is compressed: the body only arrives intact if the
			// transport negotiated gzip itself and decoded it again.
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			_, _ = writer.Write([]byte(page))
			_ = writer.Close()
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	defer server.Close()

	allowLoopback(t)
	compiled := presetBundle(t, "web-fetch")
	if compiled == nil {
		t.Skip("web-fetch is not in the catalog")
	}
	payload, err := json.Marshal(map[string]any{"name": "web_fetch", "arguments": map[string]any{"url": server.URL + "/start"}})
	if err != nil {
		t.Fatal(err)
	}
	request := workerRequest{
		Bundle: compiled.Bundle, Manifest: compiled.Manifest, Settings: compiled.Settings,
		Hook: "onToolCall", Context: Context{}, Input: payload,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	response, err := compiled.runNodeHook(WithHostCaller(ctx, localFetcher{}), request, 30*time.Second, nodePath, script)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(response.Output, &result); err != nil {
		t.Fatalf("output %s: %v", string(response.Output), err)
	}

	for _, want := range []string{
		"Source: " + server.URL + "/page", // the redirect was followed and reported
		"Title: Fetched page",
		"text/html",
		"Body text from the server",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("result is missing %q:\n%s", want, result.Content)
		}
	}
	for _, unwanted := range []string{"Nav link", "Footer text"} {
		if strings.Contains(result.Content, unwanted) {
			t.Fatalf("result kept %q:\n%s", unwanted, result.Content)
		}
	}

	// The request has to look like a navigation, not like a script.
	if !strings.Contains(seen.Get("User-Agent"), "Chrome/") {
		t.Fatalf("user agent = %q", seen.Get("User-Agent"))
	}
	if seen.Get("Accept-Language") == "" || seen.Get("Sec-Fetch-Mode") != "navigate" {
		t.Fatalf("browser headers missing: %q %q", seen.Get("Accept-Language"), seen.Get("Sec-Fetch-Mode"))
	}
	// Setting Accept-Encoding would stop Go from decoding the response, and the
	// sandbox has no inflate: the body above only reads as text because the
	// transport negotiated gzip itself and decoded it again.
	if strings.Contains(seen.Get("Accept-Encoding"), "br") {
		t.Fatalf("the extension set Accept-Encoding: %q", seen.Get("Accept-Encoding"))
	}
	if !strings.Contains(seen.Get("Accept-Encoding"), "gzip") {
		t.Fatalf("the transport did not negotiate gzip, so the decoding path was not exercised: %q", seen.Get("Accept-Encoding"))
	}
}

// localFetcher answers the executor's http.fetch host call in this process, so
// the request can reach the test server.
type localFetcher struct{}

func (localFetcher) Exec(_ context.Context, fn string, args json.RawMessage) (any, error) {
	if fn != "http.fetch" {
		return nil, nil
	}
	var call struct {
		URL     string         `json:"url"`
		Options map[string]any `json:"options"`
	}
	if err := json.Unmarshal(args, &call); err != nil {
		return nil, err
	}
	result, err := FetchAllowed([]string{"*"}, call.URL, call.Options)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":  result.Status,
		"ok":      result.Status >= 200 && result.Status < 300,
		"url":     result.FinalURL,
		"headers": HeadersToMap(result.Header),
		"body":    string(result.Body),
	}, nil
}
