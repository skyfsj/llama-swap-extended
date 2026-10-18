package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// TestProxy_ExtensionForwardCannotOverrideModel guards the allowlist check:
// models.forward validates args.model, then merges the script's request object.
// A request-side "model" must not replace the validated name.
func TestProxy_ExtensionForwardCannotOverrideModel(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default { async onRequest(ctx, request) { return request; } };`, &upstreamBody)
	s.cfg.Models["secret"] = config.ModelConfig{}
	s.routes()

	host := newExtensionHost(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	identity := auth.Identity{ID: "key-1", Models: []string{"m"}}
	host.request = host.request.WithContext(withIdentity(context.Background(), identity))

	args, _ := json.Marshal(map[string]any{
		"model": "m",
		"request": map[string]any{
			"model":    "secret",
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	})
	result, err := host.execForward(context.Background(), args)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	_ = result
	if upstreamBody == "" {
		t.Fatal("forward reached no upstream")
	}
	var forwarded map[string]any
	if err := json.Unmarshal([]byte(upstreamBody), &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded["model"] != "m" {
		t.Fatalf("forwarded model = %v; the script overrode the allowlist-checked model", forwarded["model"])
	}
	if forwarded["stream"] == true {
		t.Fatalf("forwarded stream = true; the buffered inner call must stay non-streaming")
	}
}

// TestProxy_ExtensionForwardRejectsDisallowedModel keeps the positive case:
// a model outside the caller's list never reaches dispatch.
func TestProxy_ExtensionForwardRejectsDisallowedModel(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default { async onRequest(ctx, request) { return request; } };`, &upstreamBody)
	s.cfg.Models["secret"] = config.ModelConfig{}
	s.routes()

	host := newExtensionHost(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	identity := auth.Identity{ID: "key-1", Models: []string{"m"}}
	host.request = host.request.WithContext(withIdentity(context.Background(), identity))

	args := json.RawMessage(`{"model":"secret","request":{"messages":[]}}`)
	if _, err := host.execForward(context.Background(), args); err == nil {
		t.Fatal("forward to a model outside the identity allowlist must fail")
	}
	if upstreamBody != "" {
		t.Fatalf("disallowed forward reached upstream: %s", upstreamBody)
	}
}

// TestSwaputilReplaceRequestBodyRefreshesGetBody guards the replay seam: the
// gateway retries from GetBody and the session fingerprint restores from it,
// so a rewrite that leaves a stale GetBody forwards the pre-rewrite payload.
func TestSwaputilReplaceRequestBodyRefreshesGetBody(t *testing.T) {
	original := []byte(`{"model":"local","messages":[]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(original)))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(original))), nil
	}

	rewritten := []byte(`{"model":"upstream","messages":[]}`)
	swaputil.ReplaceRequestBody(request, rewritten)

	live, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != string(rewritten) {
		t.Fatalf("live body = %s", live)
	}
	replay, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := io.ReadAll(replay)
	if string(replayed) != string(rewritten) {
		t.Fatalf("GetBody replayed %s, want the rewritten body %s", replayed, rewritten)
	}
}
