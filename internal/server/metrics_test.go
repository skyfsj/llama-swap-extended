package server

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/protocol/anthropiccache"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestMetricsMonitor_PricingReconfigureInvalidatesCache(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	model := "price-model"
	pricingRef := config.PricingRef{Provider: "fixture", Model: "fixture-model"}
	if err := mm.store.UpsertPrice(context.Background(), store.Price{
		Provider: pricingRef.Provider,
		Model:    pricingRef.Model,
		Input:    1,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{
		model: {Backend: config.BackendConfig{Pricing: pricingRef}},
	}}
	mm.configurePricing(mm.store, cfg)
	first, ok := mm.lookupPrice(model)
	if !ok || first.Input != 1 {
		t.Fatalf("initial lookup = %+v, found=%v", first, ok)
	}
	if err := mm.store.UpsertPrice(context.Background(), store.Price{
		Provider: pricingRef.Provider,
		Model:    pricingRef.Model,
		Input:    2,
	}); err != nil {
		t.Fatal(err)
	}
	uncleared, ok := mm.lookupPrice(model)
	if !ok || uncleared.Input != 1 {
		t.Fatalf("cached lookup = %+v, found=%v, want old value before reconfigure", uncleared, ok)
	}
	mm.configurePricing(mm.store, cfg)
	refreshed, ok := mm.lookupPrice(model)
	if !ok || refreshed.Input != 2 {
		t.Fatalf("refreshed lookup = %+v, found=%v, want new value", refreshed, ok)
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 20 || entry.Tokens.OutputTokens != 50 || entry.Tokens.CachedTokens != 8 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 40.0 || entry.Tokens.PromptPerSecond != 100.0 {
		t.Fatalf("rates = %+v", entry.Tokens)
	}
	if entry.DurationMs != 1450 {
		t.Fatalf("DurationMs = %d, want 1450", entry.DurationMs)
	}
}

func TestServer_ParseMetricsRejectsUnreasonableRates(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":8697384.1,"predicted_per_second":1000001}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.PromptPerSecond >= 0 || entry.Tokens.TokensPerSecond >= 0 {
		t.Fatalf("unreasonable rates were retained: %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse(t *testing.T) {
	body := []byte("data: {\"choices\":[{}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33}}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 15 || entry.Tokens.OutputTokens != 33 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ApplyObservedStreamingRates(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	entry := ActivityLogEntry{Tokens: TokenMetrics{
		CachedTokens:    20,
		InputTokens:     100,
		OutputTokens:    30,
		PromptPerSecond: -1,
		TokensPerSecond: -1,
	}}

	applyObservedStreamingRates(&entry, start, start.Add(2*time.Second), start.Add(5*time.Second))

	if got, want := entry.Tokens.PromptPerSecond, 40.0; got != want {
		t.Fatalf("prompt rate = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 10.0; got != want {
		t.Fatalf("decode rate = %v, want %v", got, want)
	}
}

func TestServer_ApplyObservedStreamingRates_PreservesBackendTelemetry(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	entry := ActivityLogEntry{Tokens: TokenMetrics{
		InputTokens:     100,
		OutputTokens:    30,
		PromptPerSecond: 123.0,
		TokensPerSecond: 45.0,
	}}

	applyObservedStreamingRates(&entry, start, start.Add(2*time.Second), start.Add(5*time.Second))

	if got, want := entry.Tokens.PromptPerSecond, 123.0; got != want {
		t.Fatalf("prompt rate = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 45.0; got != want {
		t.Fatalf("decode rate = %v, want %v", got, want)
	}
}

func TestServer_ApplyObservedStreamingRatesSkipsSubMillisecondSamples(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	entry := ActivityLogEntry{Tokens: TokenMetrics{
		InputTokens:     100,
		OutputTokens:    30,
		PromptPerSecond: -1,
		TokensPerSecond: -1,
	}}

	applyObservedStreamingRates(&entry, start, start.Add(500*time.Microsecond), start.Add(900*time.Microsecond))

	if entry.Tokens.PromptPerSecond >= 0 || entry.Tokens.TokensPerSecond >= 0 {
		t.Fatalf("unreliable sub-millisecond rates were retained: %+v", entry.Tokens)
	}
}

func TestServer_StreamingContentTimesIgnoresRoleAndWhitespaceFrames(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	roleOnly := []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
	whitespace := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" \"}}]}\n\n")
	content := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ready\"}}]}\n\n")
	lastContent := []byte("data: {\"choices\":[{\"delta\":{\"content\":\" now\"}}]}\n\n")
	body := append(append(append(roleOnly, whitespace...), content...), lastContent...)
	writes := []responseBodyWrite{
		{end: len(roleOnly), at: start.Add(time.Second)},
		{end: len(roleOnly) + len(whitespace), at: start.Add(2 * time.Second)},
		{end: len(roleOnly) + len(whitespace) + len(content), at: start.Add(5 * time.Second)},
		{end: len(body), at: start.Add(7 * time.Second)},
	}

	first, last, ok := streamingContentTimes(body, writes)
	if !ok {
		t.Fatal("expected streaming content timestamps")
	}
	if want := start.Add(5 * time.Second); !first.Equal(want) {
		t.Fatalf("first content at %v, want %v", first, want)
	}
	if want := start.Add(7 * time.Second); !last.Equal(want) {
		t.Fatalf("last content at %v, want %v", last, want)
	}
}

// TestServer_StreamingContentTimesRecognizesResponsesDeltas pins the Responses
// API shape: its per-token frames carry the text in "delta", so a detector that
// only understands chat-completion frames matches nothing but the terminal
// "*.done" events. Those are end-of-phase markers, which collapsed the measured
// decode interval to the gap between the end of reasoning and the end of the
// answer while the numerator still counted every output token — reporting a
// decode rate an order of magnitude too high.
func TestServer_StreamingContentTimesRecognizesResponsesDeltas(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	created := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n")
	reasoning := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"thinking\"}\n\n")
	answer := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n")
	done := []byte("data: {\"type\":\"response.reasoning_text.done\",\"text\":\"thinking\"}\n\n")

	body := append(append(append(append([]byte{}, created...), reasoning...), answer...), done...)
	writes := []responseBodyWrite{
		{end: len(created), at: start.Add(time.Second)},
		{end: len(created) + len(reasoning), at: start.Add(2 * time.Second)},
		{end: len(created) + len(reasoning) + len(answer), at: start.Add(6 * time.Second)},
		{end: len(body), at: start.Add(7 * time.Second)},
	}

	first, last, ok := streamingContentTimes(body, writes)
	if !ok {
		t.Fatal("expected Responses streaming content timestamps")
	}
	// The window has to start at the first generated token, not at the terminal
	// frame that happens to carry a "text" field.
	if want := start.Add(2 * time.Second); !first.Equal(want) {
		t.Fatalf("first content at %v, want %v (the first delta frame)", first, want)
	}
	if want := start.Add(7 * time.Second); !last.Equal(want) {
		t.Fatalf("last content at %v, want %v", last, want)
	}
	if decode := last.Sub(first); decode != 5*time.Second {
		t.Fatalf("decode window %v, want 5s", decode)
	}
}

func TestServer_StreamingContentTimesRecognizesNativeLlamaCompletion(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	firstContent := []byte("data: {\"content\":\"ready\"}\n\n")
	lastContent := []byte("data: {\"content\":\" now\"}\n\n")
	body := append(append([]byte{}, firstContent...), lastContent...)
	writes := []responseBodyWrite{
		{end: len(firstContent), at: start.Add(3 * time.Second)},
		{end: len(body), at: start.Add(4 * time.Second)},
	}

	first, last, ok := streamingContentTimes(body, writes)
	if !ok {
		t.Fatal("expected llama.cpp streaming content timestamps")
	}
	if want := start.Add(3 * time.Second); !first.Equal(want) {
		t.Fatalf("first content at %v, want %v", first, want)
	}
	if want := start.Add(4 * time.Second); !last.Equal(want) {
		t.Fatalf("last content at %v, want %v", last, want)
	}
}

func TestServer_ProcessStreamingResponse_VLLMMetrics(t *testing.T) {
	body := []byte(`data: {"id":"chatcmpl-b7a832cea986aea4","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":14,"total_tokens":166,"completion_tokens":152},"metrics":{"time_to_first_token_ms":70,"mean_itl_ms":10,"tokens_per_second":24.116032676555495}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 14 || entry.Tokens.OutputTokens != 152 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.CachedTokens != -1 {
		t.Errorf("CachedTokens = %d, want -1", entry.Tokens.CachedTokens)
	}
	if entry.FirstTokenMs != 70 {
		t.Errorf("FirstTokenMs = %d, want 70", entry.FirstTokenMs)
	}
	if got, want := entry.Tokens.PromptPerSecond, 200.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 24.116032676555495; math.Abs(got-want) > 1e-12 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ProcessStreamingResponse_MultilineUsage(t *testing.T) {
	body := []byte("data: {\n" +
		"data: \"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}\n" +
		"data: }\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 4 || entry.Tokens.OutputTokens != 2 {
		t.Fatalf("tokens = %+v, want 4/2", entry.Tokens)
	}
}

func TestServer_ParseMetrics_VLLMMetrics(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128,"total_tokens":170,"prompt_tokens_details":{"cached_tokens":20}},"metrics":{"time_to_first_token_ms":85.2,"generation_time_ms":1240.5,"queue_time_ms":12.3,"mean_itl_ms":9.1,"tokens_per_second":103.2}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 42 || entry.Tokens.OutputTokens != 128 || entry.Tokens.CachedTokens != 20 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.FirstTokenMs != 85 {
		t.Errorf("FirstTokenMs = %d, want 85", entry.FirstTokenMs)
	}
	if got, want := entry.Tokens.PromptPerSecond, float64(42-20)/(85.2/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 103.2; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ApplyUsageTelemetry_VLLMDetails(t *testing.T) {
	entry := ActivityLogEntry{}
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":40,"created_cache_tokens":5},"completion_tokens_details":{"reasoning_tokens":8}}}`)
	applyUsageTelemetry(&entry, body)
	if entry.Tokens.InputTokens != 100 || entry.Tokens.OutputTokens != 12 || entry.Tokens.CachedTokens != 40 {
		t.Fatalf("vLLM usage details = %+v", entry)
	}
	if entry.CacheCreationTokens != 5 || entry.ReasoningTokens != 8 {
		t.Fatalf("vLLM usage detail counters = %+v", entry)
	}
	if got, want := entry.CacheHitRatio, 0.4; math.Abs(got-want) > 1e-9 {
		t.Errorf("CacheHitRatio = %v, want %v", got, want)
	}
	if got, want := entry.CacheCreationRatio, 0.05; math.Abs(got-want) > 1e-9 {
		t.Errorf("CacheCreationRatio = %v, want %v", got, want)
	}
}

func TestServer_ParseMetrics_VLLMFlatRequestTelemetry(t *testing.T) {
	body := `{"id":"chatcmpl-qwen","object":"chat.completion","usage":{"prompt_tokens":73,"completion_tokens":256,"total_tokens":329,"prompt_tokens_details":null},"metrics":{"time_to_first_token_ms":120,"queue_time_ms":20,"prefill_time_ms":100,"generation_time_ms":6400,"cached_tokens":40}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 73 || entry.Tokens.OutputTokens != 256 || entry.Tokens.CachedTokens != 40 {
		t.Fatalf("tokens = %+v, want input=73 output=256 cached=40", entry.Tokens)
	}
	if got, want := entry.CacheHitRatio, 40.0/73.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("CacheHitRatio = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.PromptPerSecond, 33.0/(100.0/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 256.0/(6400.0/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
	if entry.FirstTokenMs != 120 {
		t.Errorf("FirstTokenMs = %d, want 120", entry.FirstTokenMs)
	}
}

func TestMetricsMonitor_RecordVLLMFlatRequestTelemetry(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{ModelID: "m"}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":73,"completion_tokens":256,"total_tokens":329,"prompt_tokens_details":null},"metrics":{"time_to_first_token_ms":120,"queue_time_ms":20,"prefill_time_ms":100,"generation_time_ms":6400,"cached_tokens":40}}`))

	mm.record("m", r, copier, nil, nil)
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Tokens.CachedTokens != 40 || entry.Tokens.InputTokens != 73 || entry.Tokens.OutputTokens != 256 {
		t.Fatalf("persisted tokens = %+v", entry.Tokens)
	}
	if got, want := entry.CacheHitRatio, 40.0/73.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("persisted CacheHitRatio = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.PromptPerSecond, 330.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("persisted PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 40.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("persisted TokensPerSecond = %v, want %v", got, want)
	}
	if entry.FirstTokenMs != 120 {
		t.Errorf("persisted FirstTokenMs = %d, want 120", entry.FirstTokenMs)
	}
}

func TestServer_ApplyUsageTelemetry_AnthropicCache(t *testing.T) {
	entry := ActivityLogEntry{}
	applyUsageTelemetry(&entry, []byte(`{"usage":{"input_tokens":20,"output_tokens":7,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`))
	if entry.Tokens.CachedTokens != 30 {
		t.Fatalf("cached tokens = %d, want 30", entry.Tokens.CachedTokens)
	}
	if entry.CacheCreationTokens != 10 {
		t.Fatalf("cache creation tokens = %d, want 10", entry.CacheCreationTokens)
	}
	if got, want := entry.CacheHitRatio, 0.5; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache hit ratio = %v, want %v", got, want)
	}
	if got, want := entry.CacheCreationRatio, 10.0/60.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache creation ratio = %v, want %v", got, want)
	}
}

func TestServer_ApplyUsageTelemetry_ExplicitUncachedPartition(t *testing.T) {
	entry := ActivityLogEntry{}
	applyUsageTelemetry(&entry, []byte(`{"usage":{"input_tokens":20,"uncached_input_tokens":7,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`))
	if entry.Tokens.InputTokens != 7 {
		t.Fatalf("input tokens = %d, want explicit uncached count 7", entry.Tokens.InputTokens)
	}
	if got, want := entry.CacheHitRatio, 30.0/47.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache hit ratio = %v, want %v", got, want)
	}
	if got, want := entry.CacheCreationRatio, 10.0/47.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache creation ratio = %v, want %v", got, want)
	}
}

func TestServer_ApplyUsageTelemetry_OpenAICache(t *testing.T) {
	entry := ActivityLogEntry{}
	applyUsageTelemetry(&entry, []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":40}}}`))
	if entry.Tokens.CachedTokens != 40 {
		t.Fatalf("cached tokens = %d, want 40", entry.Tokens.CachedTokens)
	}
	if got, want := entry.CacheHitRatio, 0.4; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache hit ratio = %v, want %v", got, want)
	}
	if entry.CacheCreationRatio != 0 {
		t.Fatalf("cache creation ratio = %v, want 0", entry.CacheCreationRatio)
	}
}

func TestServer_ApplyUsageTelemetry_CamelCaseAndNestedDetails(t *testing.T) {
	entry := ActivityLogEntry{}
	applyUsageTelemetry(&entry, []byte(`{"usage":{"inputTokens":100,"outputTokens":12,"promptTokensDetails":{"cachedTokens":40},"completionTokensDetails":{"reasoningTokens":3}}}`))
	if entry.Tokens.InputTokens != 100 || entry.Tokens.OutputTokens != 12 || entry.Tokens.CachedTokens != 40 || entry.ReasoningTokens != 3 {
		t.Fatalf("camelCase telemetry = %+v", entry)
	}
	if got, want := entry.CacheHitRatio, 0.4; math.Abs(got-want) > 1e-9 {
		t.Fatalf("camelCase cache hit ratio = %v, want %v", got, want)
	}
	if entry.CacheCreationRatio != 0 {
		t.Fatalf("camelCase cache creation ratio = %v, want 0", entry.CacheCreationRatio)
	}
}

func TestServer_ApplyUsageTelemetry_MultilineSSE(t *testing.T) {
	entry := ActivityLogEntry{}
	body := []byte("event: response.completed\n" +
		"data: {\n" +
		"data: \"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":4,\"cache_creation_input_tokens\":1}\n" +
		"data: }\n\n")
	applyUsageTelemetry(&entry, body)
	if entry.Tokens.CachedTokens != 4 || entry.CacheCreationTokens != 1 {
		t.Fatalf("cache telemetry = %+v", entry)
	}
	if got, want := entry.CacheCreationRatio, 1.0/15.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache creation ratio = %v, want %v", got, want)
	}
}

func TestServer_ApplyUsageTelemetry_AnthropicMessageUsage(t *testing.T) {
	entry := ActivityLogEntry{}
	body := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}`)
	applyUsageTelemetry(&entry, body)
	if entry.Tokens.CachedTokens != 30 || entry.CacheCreationTokens != 10 {
		t.Fatalf("message usage telemetry = %+v", entry)
	}
	if got, want := entry.CacheHitRatio, 0.5; math.Abs(got-want) > 1e-9 {
		t.Fatalf("message cache hit ratio = %v, want %v", got, want)
	}
	if got, want := entry.CacheCreationRatio, 10.0/60.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("message cache creation ratio = %v, want %v", got, want)
	}
}

func TestServer_ApplyUsageTelemetry_ClampsMalformedCounters(t *testing.T) {
	entry := ActivityLogEntry{}
	applyUsageTelemetry(&entry, []byte(`{"usage":{"input_tokens":-20,"uncached_input_tokens":-3,"cache_read_input_tokens":999999999999999999999999,"cache_creation_input_tokens":"10.0","reasoning_tokens":"bad"}}`))
	if entry.Tokens.InputTokens != 0 || entry.Tokens.CachedTokens != 0 || entry.CacheCreationTokens != 10 {
		t.Fatalf("malformed cache telemetry = %+v", entry)
	}
	if entry.CacheHitRatio < 0 || entry.CacheHitRatio > 1 {
		t.Fatalf("cache hit ratio out of bounds: %v", entry.CacheHitRatio)
	}
	if entry.CacheCreationRatio < 0 || entry.CacheCreationRatio > 1 {
		t.Fatalf("cache creation ratio out of bounds: %v", entry.CacheCreationRatio)
	}
}

func TestServer_ParseMetricsPreservesValidFieldsForMalformedUsage(t *testing.T) {
	body := `{"usage":{"prompt_tokens":"12.0","completion_tokens":"not-a-number","prompt_tokens_details":{"cached_tokens":"4"}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 0 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ApplySessionUsageAccumulatesAcrossResponses(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10)
	// The row already carries its own per-request ratios and session
	// accounting must not overwrite them with the session cumulative.
	first := ActivityLogEntry{SessionID: "session-1", CacheHitRatio: 0.25, CacheCreationRatio: 0.125}
	if !mm.applySessionUsage(&first, []byte(`{"usage":{"input_tokens":20,"uncached_input_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`)) {
		t.Fatal("first response should contain cache telemetry")
	}
	if got, want := first.CacheHitRatio, 0.25; math.Abs(got-want) > 1e-9 {
		t.Fatalf("row cache hit ratio = %v, want per-request %v", got, want)
	}
	if got, want := first.CacheCreationRatio, 0.125; math.Abs(got-want) > 1e-9 {
		t.Fatalf("row cache creation ratio = %v, want per-request %v", got, want)
	}
	second := ActivityLogEntry{SessionID: "session-1"}
	if !mm.applySessionUsage(&second, []byte("event: message_start\n"+
		"data: {\"usage\":{\"input_tokens\":30,\"uncached_input_tokens\":30,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":0}}\n\n")) {
		t.Fatal("second response should contain cache telemetry")
	}
	// Adding a zero usage probe leaves the cumulative totals unchanged.
	cumulative := mm.sessionUsage.Add("session-1", anthropiccache.Usage{})
	if got, want := cumulative.HitRatio, 40.0/100.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("session cumulative cache hit ratio = %v, want %v", got, want)
	}
	if got, want := cumulative.CreationRatio, 10.0/100.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("session cumulative cache creation ratio = %v, want %v", got, want)
	}
	if mm.applySessionUsage(&ActivityLogEntry{}, []byte(`{"usage":{"input_tokens":20,"cache_read_input_tokens":10}}`)) {
		t.Fatal("response without session should not update session telemetry")
	}
}

func TestServer_ParseMetrics_VLLMSpeculativeDecoding(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"mean_itl_ms":9.1,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"acceptance_histogram":[6,14],"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// A speculative_decoding object missing either counter must leave both unset so
// the acceptance rate is not computed from half the data.
func TestServer_ParseMetrics_VLLMSpeculativeDecodingPartial(t *testing.T) {
	body := `{"usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"speculative_decoding":{"num_draft_tokens":20}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != -1 || entry.Tokens.DraftAccTokens != -1 {
		t.Fatalf("draft tokens = %+v, want -1/-1", entry.Tokens)
	}
}

// vLLM only emits the metrics object on the final streamed chunk.
func TestServer_ProcessStreamingResponse_VLLMSpeculativeDecoding(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11742,"completion_tokens":35},"metrics":{"mean_itl_ms":17.209908293272534,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// llama-server timings and vLLM metrics never appear together, but a response
// carrying both must not have its timings-sourced draft counts clobbered.
func TestServer_ParseMetrics_TimingsDraftTokensNotOverwritten(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"draft_n":30,"draft_n_accepted":12},"metrics":{"speculative_decoding":{}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 30 || entry.Tokens.DraftAccTokens != 12 {
		t.Fatalf("draft tokens = %+v, want 12/30", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

func TestMetricsMonitor_RecordMetadata(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web", "trace": "abc"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, nil, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata["client"] != "web" {
		t.Errorf("client = %q, want web", entries[0].Metadata["client"])
	}
	if entries[0].Metadata["trace"] != "abc" {
		t.Errorf("trace = %q, want abc", entries[0].Metadata["trace"])
	}
}

func TestMetricsMonitor_RecordAnthropicCacheTransformTelemetry(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := swaputil.SetContext(r.Context(), swaputil.ReqContextData{ModelID: "m"})
	ctx = withAnthropicCacheTelemetry(ctx, anthropicCacheTelemetry{
		Applied:    true,
		Detected:   true,
		Enabled:    true,
		PrefixHash: "prefix-hash",
		Transforms: []anthropiccache.TransformResult{{Name: "fingerprint-strip", Applied: true, Reason: "removed transient cache fingerprint fields"}, {Name: "high-risk", Skipped: true, Reason: "audit-only transform"}},
		Anomalies:  []string{"high-risk cache transform was not applied"},
	})
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":2}}`))

	mm.record("m", r, copier, nil, nil)
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if !entry.RepairApplied || entry.PrefixHash != "prefix-hash" {
		t.Fatalf("cache telemetry = repair=%v prefix=%q", entry.RepairApplied, entry.PrefixHash)
	}
	if entry.Metadata["cache_fix_detected"] != "true" || entry.Metadata["cache_fix_enabled"] != "true" {
		t.Fatalf("cache detection metadata = %#v", entry.Metadata)
	}
	if !strings.Contains(entry.Metadata["cache_transforms"], "fingerprint-strip") || !strings.Contains(entry.Metadata["cache_transforms"], "audit-only transform") {
		t.Fatalf("transform metadata = %q", entry.Metadata["cache_transforms"])
	}
	if entry.Metadata["cache_anomalies"] != "high-risk cache transform was not applied" {
		t.Fatalf("anomaly metadata = %q", entry.Metadata["cache_anomalies"])
	}
}

func TestMetricsMonitor_RecordPreservesAuthenticatedKeyWithoutMetadataKeyID(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	ctx := swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"session_id": "session-1"},
	})
	r = r.WithContext(withIdentity(ctx, auth.Identity{ID: "key-1"}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, nil, nil)
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].KeyID != "key-1" {
		t.Fatalf("key id = %q, want authenticated identity key-1", entries[0].KeyID)
	}
}

// TestMetricsMonitor_RecordClientClosed covers #1029: a client that hangs up
// before a response is written must not be filed as a successful (empty-body)
// metric. It is recorded with the 499 sentinel and a client-cancelled
// ErrorMsg, and marks its audit entry incomplete when audit is enabled.
func TestMetricsMonitor_RecordClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	// Nothing is ever written: this is the cold-load cancellation shape.
	copier.MarkStatus(swaputil.StatusClientClosedRequest)

	mm.record("m", r, copier, []byte("req"), nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if entry.ErrorMsg != "client disconnected before response" {
		t.Errorf("error_msg = %q, want client-cancelled message", entry.ErrorMsg)
	}
}

// TestServer_MetricsMiddleware_ClientClosed checks the middleware derives the
// sentinel for a handler that returns without writing, and that the marker
// propagates outward to the access-log recorder so both agree.
func TestServer_MetricsMiddleware_ClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"
	cancel()

	handler.ServeHTTP(httptest.NewRecorder(), r)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("activity status = %d, want %d", entries[0].RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if line := string(proxylog.GetHistory()); !strings.Contains(line, "499 0") {
		t.Errorf("access log %q should report 499", line)
	}
}

// TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed guards the
// distinction #1029's fix depends on. The inflight middleware derives a
// cancellable context that POST /api/inflight/{id}/cancel fires, so testing
// "is this request's context done?" would report an operator cancelling a
// request from the UI as a client that hung up.
func TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	// The handler stands in for a dispatch that is cancelled mid-flight and
	// answers the still-connected client, as the proxy ErrorHandler does.
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {
		derived, cancel := context.WithCancel(r.Context())
		defer cancel()
		cancel() // the operator cancels it
		r = r.WithContext(derived)
		if swaputil.MarkClientClosed(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("client got %d, want %d: a connected client must be answered", w.Code, http.StatusBadGateway)
	}
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode == swaputil.StatusClientClosedRequest {
		t.Error("a server-side cancel must not be recorded as a client disconnect")
	}
	if line := string(proxylog.GetHistory()); strings.Contains(line, "499") {
		t.Errorf("access log %q should not report 499 for a connected client", line)
	}
}

func TestMetricsMonitor_RecordFailedRequestAudit(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	mm.configureAudit(config.AuditConfig{Enabled: true, StoreMedia: true})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqHeaders := map[string]string{"content-type": "application/json"}

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"error":{"message":"model unavailable"}}`))

	reqBody := []byte(`{"model":"m","messages":[]}`)
	mm.record("m", r, copier, reqBody, reqHeaders)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, http.StatusBadGateway)
	}
	if entry.ErrorMsg != "model unavailable" {
		t.Errorf("error_msg = %q, want extracted message", entry.ErrorMsg)
	}
	rows, err := mm.store.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v, err=%v", rows, err)
	}
	got, found, err := mm.store.GetAuditConversation(context.Background(), rows[0].ID)
	if err != nil || !found {
		t.Fatalf("audit conversation found=%v err=%v", found, err)
	}
	if string(got.RequestBody) != `{"model":"m","messages":[]}` {
		t.Errorf("request body = %q", got.RequestBody)
	}
	if string(got.ResponseBody) != `{"error":{"message":"model unavailable"}}` {
		t.Errorf("response body = %q", got.ResponseBody)
	}
	if !strings.Contains(string(got.ResponseHeaders), "Content-Type") {
		t.Errorf("response headers = %q", got.ResponseHeaders)
	}
}

func TestMetricsMonitor_RecordFailedRequestStatusFallback(t *testing.T) {
	// Non-JSON error body: ErrorMsg falls back to the HTTP status text.
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte("<html>upstream down</html>"))

	mm.record("m", r, copier, nil, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg != "502 Bad Gateway" {
		t.Errorf("error_msg = %q, want status text", entries[0].ErrorMsg)
	}
}

func TestMetricsMonitor_AuditNeverPersistsAuthenticationHeaders(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	mm.configureAudit(config.AuditConfig{Enabled: true, StoreMedia: true, RedactHeaders: false})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer request-secret")

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Set-Cookie", "session-secret")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	mm.record("m", r, copier, []byte(`{"model":"m"}`), map[string]string{"Authorization": "Bearer request-secret", "X-Trace": "keep"})

	rows, err := mm.store.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v, err=%v", rows, err)
	}
	row := rows[0]
	if row.ActivityID <= 0 {
		t.Fatalf("audit row did not retain numeric activity id: %+v", row)
	}
	if strings.Contains(string(row.RequestHeaders), "request-secret") || strings.Contains(string(row.ResponseHeaders), "session-secret") {
		t.Fatalf("authentication material leaked into audit headers: req=%s resp=%s", row.RequestHeaders, row.ResponseHeaders)
	}
	if !strings.Contains(string(row.RequestHeaders), "X-Trace") {
		t.Fatalf("non-sensitive request header was unexpectedly removed: %s", row.RequestHeaders)
	}
}

func TestMetricsMonitor_RecordDecompressionFailureSetsErrorMsg(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	mm.configureAudit(config.AuditConfig{Enabled: true, StoreMedia: true})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Encoding", "gzip")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("not-really-gzip"))

	mm.record("m", r, copier, []byte("req"), nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg == "" {
		t.Fatal("expected ErrorMsg for decompression failure")
	}
	// Raw bytes must not be stored when the body could not be decoded.
	rows, err := mm.store.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("decompression failure audit rows = %+v, err=%v", rows, err)
	}
	if rows[0].Complete || !strings.Contains(string(rows[0].ResponseHeaders), "Content-Encoding") {
		t.Fatalf("decompression failure audit row = %+v", rows[0])
	}
	full, found, err := mm.store.GetAuditConversation(context.Background(), rows[0].ID)
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	if len(full.ResponseBody) != 0 {
		t.Fatalf("decompression failure stored raw response body: %q", full.ResponseBody)
	}
}

func TestMetricsMonitor_RecordOversizedResponseSkipsPartialTelemetry(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)
	mm.configureAudit(config.AuditConfig{Enabled: true, StoreMedia: true})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	for i := 0; i <= swaputil.MaxRequestBodySize/(1<<20); i++ {
		if _, err := copier.Write(chunk); err != nil {
			t.Fatalf("write oversized response chunk %d: %v", i, err)
		}
	}
	if !copier.truncated {
		t.Fatal("response copier did not mark oversized body as truncated")
	}
	if copier.body.Len() != swaputil.MaxRequestBodySize {
		t.Fatalf("buffered body len = %d, want cap %d", copier.body.Len(), swaputil.MaxRequestBodySize)
	}
	if w.Body.Len() != len(chunk)*(swaputil.MaxRequestBodySize/(1<<20)+1) {
		t.Fatalf("client body len = %d, want complete response", w.Body.Len())
	}

	mm.record("m", r, copier, []byte(`{"model":"m"}`), nil)
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if !strings.Contains(entry.ErrorMsg, "response body exceeds") {
		t.Fatalf("error_msg = %q, want oversized-body error", entry.ErrorMsg)
	}
	rows, err := mm.store.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("oversized response audit rows = %+v, err=%v", rows, err)
	}
	if rows[0].Complete {
		t.Fatalf("oversized response audit row = %+v, want incomplete", rows[0])
	}
	full, found, err := mm.store.GetAuditConversation(context.Background(), rows[0].ID)
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	if len(full.ResponseBody) != 0 {
		t.Fatalf("oversized response stored a partial response body (len=%d)", len(full.ResponseBody))
	}
}

func TestMetricsMonitor_DecodeResponseBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10)

	// No Content-Encoding: body returned unchanged.
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Write([]byte("plain"))
	got, err := mm.decodeResponseBody(copier, "/p")
	if err != nil || string(got) != "plain" {
		t.Fatalf("plain body = %q, err = %v", got, err)
	}

	// Bogus gzip payload: returns an error and no body (no raw bytes kept).
	w2 := httptest.NewRecorder()
	copier2 := newBodyCopier(w2)
	copier2.Header().Set("Content-Encoding", "gzip")
	copier2.Write([]byte("not-really-gzip"))
	got, err = mm.decodeResponseBody(copier2, "/p")
	if err == nil {
		t.Fatal("expected decompression error")
	}
	if got != nil {
		t.Errorf("expected nil body on failure, got %q", got)
	}
}

func TestServer_ExtractErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai object", `{"error":{"message":"rate limited"}}`, "rate limited"},
		{"string error", `{"error":"bad request"}`, "bad request"},
		{"message field", `{"message":"nope"}`, "nope"},
		{"detail field", `{"detail":"oops"}`, "oops"},
		{"object error ignored", `{"error":{"code":42}}`, ""},
		{"no error", `{"usage":{}}`, ""},
		{"invalid json", `not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServer_ParseMetrics_Infill(t *testing.T) {
	// /infill responses are arrays; timings live in the last element.
	body := `[{"content":"a"},{"content":"b","timings":{"prompt_n":5,"predicted_n":9,"prompt_ms":10,"predicted_ms":20}}]`
	parsed := gjson.Parse(body)
	timings := parsed.Get("timings")
	if arr := parsed.Array(); len(arr) > 0 {
		timings = arr[len(arr)-1].Get("timings")
	}
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings, parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

// TestServer_MetricsMiddleware_UpstreamAudioAuditSkipsMediaBody verifies that
// audit can retain media response metadata without storing binary media.
func TestServer_MetricsMiddleware_UpstreamAudioAuditSkipsMediaBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100)
	mm.configureAudit(config.AuditConfig{Enabled: true, StoreMedia: false})
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BINARY-AUDIO-DATA"))
	})
	handler := CreateMetricsMiddleware(mm, cfg)(inner)

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/audio/speech", strings.NewReader(`{"model":"m1"}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rows, err := mm.store.ListAuditConversations(context.Background(), store.AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v, err=%v", rows, err)
	}
	conversation, found, err := mm.store.GetAuditConversation(context.Background(), rows[0].ID)
	if err != nil || !found {
		t.Fatalf("audit conversation found=%v err=%v", found, err)
	}
	if len(conversation.ResponseBody) != 0 {
		t.Errorf("response body stored for media request (len=%d)", len(conversation.ResponseBody))
	}
	if !strings.Contains(string(conversation.ResponseHeaders), "Content-Type") {
		t.Errorf("response headers = %q", conversation.ResponseHeaders)
	}
}

// The per-response write-timestamp log is bounded: a token-stream response
// with thousands of Write calls must not accumulate unbounded records, while
// the head and tail segments survive so StreamingContentTimes can still map
// the first and final content frames.
func TestServer_MetricsBodyCopierBoundsWriteRecords(t *testing.T) {
	w := newBodyCopier(httptest.NewRecorder())
	frame := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	for i := 0; i < responseWriteCompactAt*2; i++ {
		if _, err := w.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.writes) > responseWriteCompactAt+responseWriteTailRecords {
		t.Fatalf("write records = %d, want bounded near %d", len(w.writes), responseWriteCompactAt)
	}
	first, last, ok := w.StreamingContentTimes()
	if !ok {
		t.Fatal("StreamingContentTimes must still work with a compacted write log")
	}
	if !first.Before(last) && first.Equal(last) {
		// A same-timestamp pair is legal on fast machines; the assertion that
		// matters is ok==true with non-zero times.
		if first.IsZero() || last.IsZero() {
			t.Fatalf("content times zero: first=%v last=%v", first, last)
		}
	}
}

// TestMetrics_FirstUsageIntFallsBackPastUnusableAlias pins the alias walk.
// Callers pass ordered aliases for the same logical field, so a path that is
// present but unusable (an explicit null) must not stop the search: returning
// there reported "unknown" even though a later alias carried the value.
func TestMetrics_FirstUsageIntFallsBackPastUnusableAlias(t *testing.T) {
	usage := gjson.Parse(`{"input_tokens": null, "inputTokens": "not-a-number", "prompt_tokens": 42}`)
	if got := firstUsageInt(usage, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens"); got != 42 {
		t.Fatalf("firstUsageInt = %d, want 42 from the last alias", got)
	}

	// Nothing usable anywhere still reports unknown.
	empty := gjson.Parse(`{"input_tokens": null}`)
	if got := firstUsageInt(empty, "input_tokens", "prompt_tokens"); got != -1 {
		t.Fatalf("firstUsageInt = %d, want -1 when no alias carries a value", got)
	}
}

// TestMetrics_FailedErrorMessageTruncatesOnRuneBoundary pins the byte-budget
// truncation. Cutting mid-rune leaves an invalid byte that JSON renders as
// \ufffd, showing the operator a corrupted character in the dashboard.
func TestMetrics_FailedErrorMessageTruncatesOnRuneBoundary(t *testing.T) {
	// 499 ASCII bytes, then a multi-byte rune straddling the 500-byte budget.
	body := []byte(`{"error":{"message":"` + strings.Repeat("a", 499) + "é" + strings.Repeat("b", 20) + `"}}`)
	msg := failedErrorMessage(http.StatusBadGateway, body, nil)
	if !utf8.ValidString(msg) {
		t.Fatalf("truncated message is not valid UTF-8: %q", msg)
	}
	if !strings.HasSuffix(msg, "...") {
		t.Fatalf("message = %q, want a truncation suffix", msg)
	}
}
