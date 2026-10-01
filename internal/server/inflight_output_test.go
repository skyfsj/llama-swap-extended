package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_InflightOutputParserExtractsOpenAIChunks(t *testing.T) {
	parser := inflightOutputParser{}
	chunks := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"reasoning_content":"先想"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"你好"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"，世界"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, chunk := range chunks {
		parser.Append([]byte(chunk))
	}

	if got, want := parser.preview, "先想你好，世界"; got != want {
		t.Fatalf("preview = %q, want %q", got, want)
	}
}

func TestServer_InflightOutputParserExtractsExplicitTelemetry(t *testing.T) {
	parser := inflightOutputParser{}
	parser.Append([]byte("data: {\"content\":\"hello\",\"timings\":{\"prompt_n\":100,\"predicted_n\":12,\"cache_n\":80,\"prompt_per_second\":25.5,\"predicted_per_second\":6.25}}\n\n"))
	parser.Append([]byte("data: {\"metrics\":{\"time_to_first_token_ms\":120}}\n\n"))

	telemetry := parser.Telemetry()
	if !telemetry.hasInput || telemetry.inputTokens != 100 {
		t.Fatalf("input telemetry = %+v", telemetry)
	}
	if !telemetry.hasOutput || telemetry.outputTokens != 12 {
		t.Fatalf("output telemetry = %+v", telemetry)
	}
	if !telemetry.hasCached || telemetry.cachedTokens != 80 {
		t.Fatalf("cache telemetry = %+v", telemetry)
	}
	if !telemetry.hasPromptRate || telemetry.promptPerSecond != 25.5 {
		t.Fatalf("prompt rate telemetry = %+v", telemetry)
	}
	if !telemetry.hasTokenRate || telemetry.tokensPerSecond != 6.25 {
		t.Fatalf("token rate telemetry = %+v", telemetry)
	}
	if !telemetry.hasFirstToken || telemetry.firstTokenMs != 120 {
		t.Fatalf("first token telemetry = %+v", telemetry)
	}
}

func TestServer_InflightOutputParserHandlesFragmentedSSEAndJSON(t *testing.T) {
	parser := inflightOutputParser{}
	parser.Append([]byte(`data: {"choices":[{"delta":{"content":"frag`))
	parser.Append([]byte(`ment"}}]}` + "\n\n"))
	if got, want := parser.preview, "fragment"; got != want {
		t.Fatalf("fragmented preview = %q, want %q", got, want)
	}

	direct := inflightOutputParser{}
	direct.Append([]byte(`{"choices":[{"message":{"content":"complete"}}]}`))
	if got, want := direct.preview, "complete"; got != want {
		t.Fatalf("direct JSON preview = %q, want %q", got, want)
	}
}

func TestServer_InflightOutputParserBoundsPreview(t *testing.T) {
	parser := inflightOutputParser{}
	parser.Append([]byte(`{"choices":[{"delta":{"content":"` + strings.Repeat("a", 700) + `"}}]}`))

	runes := []rune(parser.preview)
	if len(runes) != inflightOutputPreviewRunes || runes[0] != '…' {
		t.Fatalf("preview length/prefix = %d/%q, want %d/ellipsis", len(runes), parser.preview[:1], inflightOutputPreviewRunes)
	}
}

func TestServer_InflightTrackerPublishesOutputPreview(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 4)
	tracker := newInflightTrackerWithPublisher(4, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})
	id := tracker.Add(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), func() {})
	_ = waitInflightEvent(t, events, inflightOperationUpsert)

	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"实时输出\"}}]}\n\n")
	tracker.observeResponseBytes(id, len(payload), payload)
	update := waitInflightEventPredicate(t, events, func(update swaputil.InFlightRequestsEvent) bool {
		return update.Operation == inflightOperationUpsert && update.Request != nil && update.Request.OutputPreview != ""
	})
	if got, want := update.Request.OutputPreview, "实时输出"; got != want {
		t.Fatalf("output preview = %q, want %q", got, want)
	}
	tracker.Remove(id)
}

func TestServer_InflightTrackerPublishesExplicitTelemetry(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 4)
	tracker := newInflightTrackerWithPublisher(4, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})
	id := tracker.Add(httptest.NewRequest(http.MethodPost, "/v1/completion", nil), func() {})
	_ = waitInflightEvent(t, events, inflightOperationUpsert)

	payload := []byte("data: {\"content\":\"x\",\"timings\":{\"prompt_n\":40,\"predicted_n\":3,\"prompt_per_second\":20,\"predicted_per_second\":5}}\n\n")
	tracker.observeResponseBytes(id, len(payload), payload)
	update := waitInflightEventPredicate(t, events, func(update swaputil.InFlightRequestsEvent) bool {
		return update.Operation == inflightOperationUpsert && update.Request != nil && update.Request.TokensPerSecond == 5
	})
	if update.Request.InputTokens != 40 || update.Request.OutputTokens != 3 || update.Request.PromptPerSecond != 20 {
		t.Fatalf("telemetry entry = %+v", update.Request)
	}
	tracker.Remove(id)
}

func TestServer_InflightTrackerRemovePublishesFinalTelemetry(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 8)
	tracker := newInflightTrackerWithPublisher(8, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})
	id := tracker.Add(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), func() {})
	_ = waitInflightEvent(t, events, inflightOperationUpsert)

	// The metrics object arrives in the final streamed chunk. Remove is called
	// immediately afterwards, before the normal byte-update throttle expires.
	payload := []byte("data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":20}},\"metrics\":{\"prefill_time_ms\":200,\"generation_time_ms\":600}}\n\n")
	tracker.observeResponseBytes(id, len(payload), payload)
	tracker.Remove(id)

	final := waitInflightEventPredicate(t, events, func(update swaputil.InFlightRequestsEvent) bool {
		return update.Operation == inflightOperationUpsert && update.Request != nil && update.Request.PromptPerSecond > 0 && update.Request.TokensPerSecond > 0
	})
	if got, want := final.Request.PromptPerSecond, 400.0; got != want {
		t.Errorf("final prefill rate = %v, want %v", got, want)
	}
	if got, want := final.Request.TokensPerSecond, 5.0; got != want {
		t.Errorf("final decode rate = %v, want %v", got, want)
	}
	if final.Request.InputTokens != 100 || final.Request.OutputTokens != 3 || final.Request.CachedTokens != 20 {
		t.Errorf("final telemetry = %+v", final.Request)
	}

	removed := waitInflightEvent(t, events, inflightOperationRemove)
	if removed.ID != id {
		t.Errorf("removed id = %q, want %q", removed.ID, id)
	}
}

func TestServer_ApplyInflightCurrentRatesUsesCounterDeltas(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	entry := swaputil.InflightRequestEntry{
		Timestamp: start,
	}
	samples := inflightRateSamples{}

	first := inflightTokenTelemetry{
		inputTokens:  20,
		cachedTokens: 5,
		outputTokens: 3,
		hasInput:     true,
		hasCached:    true,
		hasOutput:    true,
	}
	applyInflightCurrentRates(&entry, first, start, &samples)
	if entry.PromptPerSecond != 0 || entry.TokensPerSecond != 0 {
		t.Fatalf("first counter sample produced a rate: %+v", entry)
	}

	second := inflightTokenTelemetry{
		inputTokens:  50,
		cachedTokens: 5,
		outputTokens: 13,
		hasInput:     true,
		hasCached:    true,
		hasOutput:    true,
	}
	applyInflightCurrentRates(&entry, second, start.Add(2*time.Second), &samples)

	if got, want := entry.PromptPerSecond, 15.0; got != want {
		t.Errorf("current prefill rate = %v, want %v", got, want)
	}
	if got, want := entry.TokensPerSecond, 5.0; got != want {
		t.Errorf("current decode rate = %v, want %v", got, want)
	}
}
