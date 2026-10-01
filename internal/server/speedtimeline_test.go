package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestStreamingSpeedTimeline_ScalesToFinalUsage(t *testing.T) {
	start := time.Now()
	var body []byte
	var writes []responseBodyWrite
	add := func(delay time.Duration, payload string) {
		body = append(body, payload...)
		writes = append(writes, responseBodyWrite{end: len(body), at: start.Add(delay)})
	}
	add(10*time.Millisecond, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	// First phase: 70 runes over ~1s.
	for i := 0; i < 10; i++ {
		add(time.Duration(110+i*100)*time.Millisecond, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"1234567\"}}]}\n\n")
	}
	// Second phase: the same runes in half the interval.
	for i := 0; i < 10; i++ {
		add(time.Duration(1210+i*50)*time.Millisecond, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"1234567\"}}]}\n\n")
	}
	// Final usage frame anchors the total.
	add(1800*time.Millisecond, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":42}}\n\n")
	add(1810*time.Millisecond, "data: [DONE]\n\n")

	encoded := streamingSpeedTimeline(body, writes, start, 42)
	if encoded == "" {
		t.Fatal("empty timeline")
	}
	var points [][2]float64
	if err := json.Unmarshal([]byte(encoded), &points); err != nil {
		t.Fatalf("timeline is not a [t, tokens] array: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("no points")
	}
	last := points[len(points)-1]
	if last[1] != 42 {
		t.Fatalf("last point = %v, want the final usage total 42", last[1])
	}
	// Monotonic in both axes.
	for i := 1; i < len(points); i++ {
		if points[i][0] < points[i-1][0] {
			t.Fatalf("time went backwards at %d: %v -> %v", i, points[i-1][0], points[i][0])
		}
		if points[i][1] < points[i-1][1] {
			t.Fatalf("tokens went backwards at %d: %v -> %v", i, points[i-1][1], points[i][1])
		}
	}
	// Both phases produced the same token mass, but the second ran in half
	// the time — the per-interval speed must show it.
	series := speedSeriesForTest(points)
	firstTps := series[(len(series)*1)/4].tps
	secondTps := series[(len(series)*3)/4].tps
	if secondTps <= firstTps {
		t.Fatalf("expected the faster second phase to show higher t/s (%v vs %v)", secondTps, firstTps)
	}
}

// speedSeriesForTest mirrors the UI-side speedSeries so the Go tests verify
// the same interval math the chart renders.
type speedPoint struct {
	t   float64
	tps float64
}

func speedSeriesForTest(points [][2]float64) []speedPoint {
	out := []speedPoint{}
	for i := 1; i < len(points); i++ {
		dt := points[i][0] - points[i-1][0]
		dTok := points[i][1] - points[i-1][1]
		if dt < 1 || dTok < 0 {
			continue
		}
		out = append(out, speedPoint{t: (points[i-1][0] + points[i][0]) / 2, tps: dTok / dt * 1000})
	}
	return out
}

func TestStreamingSpeedTimeline_FallbackToOutputTokens(t *testing.T) {
	start := time.Now()
	var body []byte
	var writes []responseBodyWrite
	add := func(delay time.Duration, payload string) {
		body = append(body, payload...)
		writes = append(writes, responseBodyWrite{end: len(body), at: start.Add(delay)})
	}
	add(50*time.Millisecond, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello world\"}}]}\n\n")
	add(150*time.Millisecond, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"again\"}}]}\n\n")

	encoded := streamingSpeedTimeline(body, writes, start, 7)
	var points [][2]float64
	if err := json.Unmarshal([]byte(encoded), &points); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("want 2 points, got %d", len(points))
	}
	if points[len(points)-1][1] != 7 {
		t.Fatalf("endpoint = %v, want output token fallback 7", points[len(points)-1][1])
	}
}

func TestStreamingSpeedTimeline_EmptyForNonTextStream(t *testing.T) {
	start := time.Now()
	var body []byte
	var writes []responseBodyWrite
	body = append(body, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"...)
	writes = append(writes, responseBodyWrite{end: len(body), at: start.Add(10 * time.Millisecond)})
	if encoded := streamingSpeedTimeline(body, writes, start, 5); encoded != "" {
		t.Fatalf("expected empty timeline for a stream with no text frames, got %s", encoded)
	}
	if encoded := streamingSpeedTimeline(nil, writes, start, 5); encoded != "" {
		t.Fatalf("expected empty timeline for an empty body, got %s", encoded)
	}
}

// Regression guard for the collapsed-window rates: a 2ms decode window must
// not produce a rate at all, and any rate above the physical decode cap must
// be discarded rather than averaged into the dashboard.
func TestMetricDecodeRateCaps(t *testing.T) {
	if got := metricDecodeRate(750 / 0.002); got != -1 {
		t.Fatalf("2ms window rate = %v, want -1", got)
	}
	if got := metricDecodeRate(45); got != 45 {
		t.Fatalf("sane rate = %v, want 45", got)
	}
	if got := metricDecodeRate(2001); got != -1 {
		t.Fatalf("above-cap rate = %v, want -1", got)
	}
	// Prefix-cache hits can legitimately reach tens of thousands of tokens
	// per second on the prompt side; the decode cap must not apply there.
	if got := metricPromptRate(70000); got != 70000 {
		t.Fatalf("cache-hit prompt rate = %v, want 70000", got)
	}
	if got := metricPromptRate(150001); got != -1 {
		t.Fatalf("above-cap prompt rate = %v, want -1", got)
	}
}

// The persisted row must round-trip the new telemetry end to end.
func TestActivitySpeedTimelineRoundTrip(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	entry := store.ActivityLogEntry{
		Model:         "m1",
		ReqPath:       "/v1/chat/completions",
		DurationMs:    5000,
		FirstTokenMs:  1200,
		DecodeMs:      3600,
		SpeedTimeline: "[[0,0],[1200,3],[4800,50]]",
		Tokens:        store.TokenMetrics{InputTokens: 100, OutputTokens: 50},
	}
	saved, err := s.store.InsertActivity(context.Background(), entry)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, ok, err := s.store.GetActivity(context.Background(), saved.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.DecodeMs != 3600 {
		t.Fatalf("decode ms = %v, want 3600", got.DecodeMs)
	}
	if got.SpeedTimeline == "" || !json.Valid([]byte(got.SpeedTimeline)) {
		t.Fatalf("timeline lost or invalid: %q", got.SpeedTimeline)
	}

	// Without an audit_conversations row the detail lookup simply misses.
	if _, ok, err := s.store.GetAuditConversationByActivityID(context.Background(), saved.ID); ok || err != nil {
		t.Fatalf("expected no conversation row, ok=%v err=%v", ok, err)
	}
}
