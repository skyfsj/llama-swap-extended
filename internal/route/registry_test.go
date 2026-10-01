package route

import (
	"net/http"
	"testing"
)

func TestRoute_DefaultDescriptorsIncludeResponsesAffinityAndRealtime(t *testing.T) {
	r, err := NewRegistry(DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	responses, ok := r.Lookup(http.MethodGet, "/v1/responses/{response_id}")
	if !ok || !responses.NeedsAffinity {
		t.Fatalf("responses retrieval descriptor = %+v, found=%v", responses, ok)
	}
	realtime, ok := r.Lookup(MethodWebSocket, "/v1/realtime")
	if !ok || realtime.Body != BodyWebSocket {
		t.Fatalf("realtime descriptor = %+v, found=%v", realtime, ok)
	}
}

func TestRoute_DefaultDescriptorsCoverVLLMServingFamilies(t *testing.T) {
	r, err := NewRegistry(DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method     string
		pattern    string
		body       BodyKind
		capability string
	}{
		{http.MethodPost, "/v1/batches", BodyJSON, "chat"},
		{http.MethodPost, "/v1/classify", BodyJSON, "classify"},
		{http.MethodPost, "/v1/pooling", BodyJSON, "pooling"},
		{http.MethodPost, "/v1/generative_scoring", BodyJSON, "generative_scoring"},
		{http.MethodPost, "/v1/embed", BodyJSON, "embed"},
		{http.MethodPost, "/v1/score", BodyJSON, "score"},
		{http.MethodPost, "/v1/rerank", BodyJSON, "rerank"},
		{http.MethodPost, "/v1/audio/transcriptions", BodyForm, "transcriptions"},
		{http.MethodPost, "/v1/audio/translations", BodyForm, "translations"},
		{http.MethodPost, "/v1/messages/count_tokens", BodyJSON, "anthropic"},
	}
	for _, tc := range cases {
		d, ok := r.Lookup(tc.method, tc.pattern)
		if !ok {
			t.Errorf("missing descriptor %s %s", tc.method, tc.pattern)
			continue
		}
		if d.Body != tc.body || d.Capability != tc.capability || d.ModelField != "model" {
			t.Errorf("descriptor %s %s = %+v", tc.method, tc.pattern, d)
		}
	}
}

func TestRoute_BatchChatAllowsOptionalModel(t *testing.T) {
	r, err := NewRegistry(DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := r.Lookup(http.MethodPost, "/v1/chat/completions/batch")
	if !ok {
		t.Fatal("missing vLLM batch chat descriptor")
	}
	if !d.ModelOptional || d.ModelField != "model" || d.ModelLocation != "body" {
		t.Fatalf("batch chat descriptor = %+v, want optional body model", d)
	}
}

func TestRoute_RejectsDuplicateAndUnprotectedManagement(t *testing.T) {
	if _, err := NewRegistry([]RouteDescriptor{{Method: http.MethodGet, Pattern: "/x"}, {Method: http.MethodGet, Pattern: "/x"}}); err == nil {
		t.Fatal("expected duplicate route error")
	}
	if _, err := NewRegistry([]RouteDescriptor{{Method: http.MethodPost, Pattern: "/admin", Management: true}}); err == nil {
		t.Fatal("expected management permission error")
	}
	if _, err := NewRegistry([]RouteDescriptor{{Method: http.MethodPost, Pattern: "/optional", Body: BodyJSON, ModelOptional: true, Permission: "inference"}}); err == nil {
		t.Fatal("expected optional model descriptor to require a model field")
	}
}

func TestRoute_RejectsMalformedPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{name: "relative", pattern: "v1/chat/completions"},
		{name: "query", pattern: "/v1/chat?stream=true"},
		{name: "fragment", pattern: "/v1/chat#fragment"},
		{name: "unclosed placeholder", pattern: "/v1/{response_id"},
		{name: "invalid placeholder name", pattern: "/v1/{1response}"},
		{name: "catch all not final", pattern: "/upstream/{path...}/tail"},
		{name: "duplicate placeholder", pattern: "/v1/{id}/related/{id}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry([]RouteDescriptor{{Method: http.MethodGet, Pattern: tc.pattern, Body: BodyQuery, Permission: "inference"}}); err == nil {
				t.Fatalf("pattern %q unexpectedly accepted", tc.pattern)
			}
		})
	}
}

func TestRoute_AcceptsBinaryBodyDescriptor(t *testing.T) {
	r, err := NewRegistry([]RouteDescriptor{{
		Method:        http.MethodPost,
		Pattern:       "/v1/audio/raw",
		Body:          BodyBinary,
		ModelField:    "model",
		ModelLocation: "query",
		Permission:    "inference",
	}})
	if err != nil {
		t.Fatalf("binary body descriptor rejected: %v", err)
	}
	d, ok := r.Lookup(http.MethodPost, "/v1/audio/raw")
	if !ok || d.Body != BodyBinary {
		t.Fatalf("binary body descriptor = %+v, found=%v", d, ok)
	}
}

func TestRoute_MatchResolvesParameterizedAndCatchAllPaths(t *testing.T) {
	r, err := NewRegistry([]RouteDescriptor{
		{Method: http.MethodGet, Pattern: "/v1/responses/{response_id}", Body: BodyQuery, Permission: "inference"},
		{Method: http.MethodGet, Pattern: "/upstream/{upstream_path...}", Body: BodyQuery, Permission: "inference"},
		{Method: http.MethodGet, Pattern: "/health", Body: BodyQuery, Permission: "inference"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, values, ok := r.Match(http.MethodGet, "/v1/responses/r%2F1")
	if !ok || d.Pattern != "/v1/responses/{response_id}" || values["response_id"] != "r/1" {
		t.Fatalf("parameter match = descriptor=%+v values=%v found=%v", d, values, ok)
	}
	d, values, ok = r.Match(http.MethodGet, "/upstream/model/v1/chat/completions")
	if !ok || d.Pattern != "/upstream/{upstream_path...}" || values["upstream_path"] != "model/v1/chat/completions" {
		t.Fatalf("catch-all match = descriptor=%+v values=%v found=%v", d, values, ok)
	}
	d, values, ok = r.Match(http.MethodGet, "/health/")
	if !ok || d.Pattern != "/health" || len(values) != 0 {
		t.Fatalf("exact match = descriptor=%+v values=%v found=%v", d, values, ok)
	}
	if _, _, ok := r.Match(http.MethodPost, "/health"); ok {
		t.Fatal("method mismatch unexpectedly matched")
	}
}

func TestRoute_MatchRejectsMalformedEscapedPathValue(t *testing.T) {
	r, err := NewRegistry([]RouteDescriptor{{Method: http.MethodGet, Pattern: "/v1/responses/{response_id}", Body: BodyQuery, Permission: "inference"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.Match(http.MethodGet, "/v1/responses/%zz"); ok {
		t.Fatal("malformed escaped path unexpectedly matched")
	}
}
