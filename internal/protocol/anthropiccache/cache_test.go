package anthropiccache

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestDetectClaudeCodeRequiresSignalAndShape(t *testing.T) {
	body := map[string]any{"system": "x", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if DetectClaudeCode(http.Header{"User-Agent": []string{"anthropic-sdk/1"}}, body) {
		t.Fatal("generic SDK should not be detected")
	}
	if !DetectClaudeCode(http.Header{"User-Agent": []string{"claude-code/2.1"}}, body) {
		t.Fatal("Claude Code should be detected")
	}
	if DetectClaudeCode(http.Header{"X-Claude-Code-Version": []string{"2.1"}}, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}) {
		t.Fatal("a client signal without Anthropic context should not be detected")
	}
	if !DetectClaudeCode(http.Header{"X-Claude-Code-Session-ID": []string{"session-1"}}, body) {
		t.Fatal("Claude Code session header should be detected with a shaped request")
	}
	typedBody := map[string]any{
		"system":   []any{map[string]any{"type": "text", "text": "rules"}},
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	if !DetectClaudeCode(http.Header{"X-Claude-Code": []string{"true"}}, typedBody) {
		t.Fatal("typed message values should be recognized as Claude Code-shaped requests")
	}
}

func TestApplyRejectsUnknownMode(t *testing.T) {
	_, err := Apply([]byte(`{"messages":[]}`), nil, Options{Mode: Mode("automatic")})
	if err == nil || !strings.Contains(err.Error(), "unsupported anthropic cache mode") {
		t.Fatalf("unknown mode error = %v", err)
	}
}

func TestApplyStabilizesBillingFingerprint(t *testing.T) {
	baseVersion := "2.1.117"
	message := "please inspect the changed files"
	legacyFingerprint := computeFingerprint(message, baseVersion)
	input := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=` + baseVersion + `.` + legacyFingerprint + `"}],"messages":[{"role":"user","content":"` + message + `"}],"tools":[{"name":"shell"}]}`)
	result, err := Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto, Transforms: TransformConfig{FingerprintStrip: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Transforms[0].Applied || !strings.Contains(string(result.Body), "cc_version="+baseVersion+"."+legacyFingerprint) {
		t.Fatalf("already stable fingerprint changed: output=%s telemetry=%+v", result.Body, result.Transforms)
	}

	otherMessage := "a different first user turn"
	old := computeFingerprint(message, baseVersion)
	input = []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=` + baseVersion + `.` + old + `"}],"messages":[{"role":"user","content":"` + otherMessage + `"}],"tools":[{"name":"shell"}]}`)
	result, err = Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto, Transforms: TransformConfig{FingerprintStrip: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("unverified fingerprint should remain unchanged: %+v body=%s", result, result.Body)
	}

	legacy := "<system-reminder>legacy context</system-reminder>"
	real := "current user text"
	legacyFingerprint = computeFingerprint(legacy, baseVersion)
	stable := computeFingerprint(real, baseVersion)
	input = []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=` + baseVersion + `.` + legacyFingerprint + `"}],"messages":[{"role":"user","content":"<system-reminder>legacy context</system-reminder>"},{"role":"user","content":"` + real + `"}],"tools":[{"name":"shell"}]}`)
	result, err = Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto, Transforms: TransformConfig{FingerprintStrip: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !strings.Contains(string(result.Body), baseVersion+"."+stable) || strings.Contains(string(result.Body), baseVersion+"."+legacyFingerprint) {
		t.Fatalf("expected verified fingerprint rewrite: changed=%v body=%s", result.Changed, result.Body)
	}

	// If there is no non-reminder user text, the legacy first-user block is
	// still the verified fingerprint source and must remain stable.
	legacyOnly := "<system-reminder>legacy context</system-reminder>"
	legacyFingerprint = computeFingerprint(legacyOnly, baseVersion)
	input = []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=` + baseVersion + `.` + legacyFingerprint + `"}],"messages":[{"role":"user","content":"` + legacyOnly + `"}],"tools":[{"name":"shell"}]}`)
	result, err = Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto, Transforms: TransformConfig{FingerprintStrip: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || !strings.Contains(string(result.Body), baseVersion+"."+legacyFingerprint) {
		t.Fatalf("legacy-only fingerprint should remain stable: changed=%v body=%s", result.Changed, result.Body)
	}
}

func TestApplyThinkingSanitizeDropsOmittedHistoryButPreservesToolContinuation(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed"},{"type":"text","text":"old"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed"},{"type":"tool_use","id":"call-1","name":"shell","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]}
	],"system":"rules"}`)
	result, err := Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto, Transforms: TransformConfig{ThinkingSanitize: "safe"}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	firstContent := messages[0].(map[string]any)["content"].([]any)
	if len(firstContent) != 1 || firstContent[0].(map[string]any)["type"] != "text" {
		t.Fatalf("omitted historical thinking was not removed: %#v", firstContent)
	}
	activeContent := messages[1].(map[string]any)["content"].([]any)
	if len(activeContent) != 2 || activeContent[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("active tool continuation thinking was modified: %#v", activeContent)
	}
	if !result.Changed || result.Anomalies == nil {
		t.Fatalf("missing thinking telemetry: %+v", result)
	}
}

func TestApplyCacheControlCanonicalizesUserMarkers(t *testing.T) {
	input := []byte(`{"system":"rules","messages":[
		{"role":"user","content":[{"type":"text","text":"one","cache_control":{"type":"invalid"}}]},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":[{"type":"text","text":"two"},{"type":"text","text":"three","cache_control":{"type":"ephemeral","ttl":"1h"}}]}
	],"tools":[{"name":"z"},{"name":"a"}]}`)
	result, err := Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	firstBlock := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, exists := firstBlock["cache_control"]; exists {
		t.Fatalf("scattered cache marker remained: %#v", firstBlock)
	}
	lastBlocks := messages[2].(map[string]any)["content"].([]any)
	marker := lastBlocks[1].(map[string]any)["cache_control"].(map[string]any)
	if marker["type"] != "ephemeral" || marker["ttl"] != "1h" {
		t.Fatalf("canonical marker=%#v", marker)
	}
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "a" || tools[1].(map[string]any)["name"] != "z" {
		t.Fatalf("tools not stabilized: %#v", tools)
	}
	second, err := Apply(result.Body, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("cache normalization is not idempotent: %+v", second.Transforms)
	}
}

func TestApplyIsIdempotentAndReportsTransforms(t *testing.T) {
	input := []byte(`{"model":"m","metadata":{"session_id":"abc","user_id":""},"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral","ttl":"bad"}}],"tools":[{"name":"z"},{"name":"a"}],"messages":[{"role":"user","content":[{"type":"thinking","thinking":"x","signature":123}]}]}`)
	first, err := Apply(input, http.Header{"User-Agent": []string{"claude-code/2"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Apply(first.Body, http.Header{"User-Agent": []string{"claude-code/2"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(first.Body, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body, &b); err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	if string(left) != string(right) {
		t.Fatalf("not idempotent:\n%s\n%s", left, right)
	}
	if first.PrefixHash == "" || !first.Changed || len(first.Transforms) == 0 {
		t.Fatalf("missing telemetry: %+v", first)
	}
	if !strings.Contains(string(first.Body), `"ttl":"1h"`) || strings.Contains(string(first.Body), `"session_id"`) {
		t.Fatalf("normalization missing: %s", first.Body)
	}
}

func TestApplyTTLUsesExplicitFiveMinuteTier(t *testing.T) {
	input := []byte(`{"system":[{"type":"text","text":"rules"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	result, err := Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Body), `"ttl":"1h"`) {
		t.Fatalf("default TTL should be one hour: %s", result.Body)
	}
	input = []byte(`{"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	result, err = Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Body), `"ttl":"5m"`) {
		t.Fatalf("existing five-minute tier should be honored: %s", result.Body)
	}
}

func TestStabilizeSortCanonicalizesReminderText(t *testing.T) {
	body := map[string]any{
		"system": []any{map[string]any{
			"type": "text",
			"text": "<system-reminder>\nThe following skills are available\n\n- z\n- a\n</system-reminder>",
		}},
		"tools": []any{map[string]any{"name": "z"}, map[string]any{"name": "a"}},
	}
	changed, _, anomaly := stabilizeSort(body)
	if !changed || anomaly != "" {
		t.Fatalf("stabilizeSort changed=%v anomaly=%q", changed, anomaly)
	}
	if body["tools"].([]any)[0].(map[string]any)["name"] != "a" {
		t.Fatalf("tools were not sorted: %#v", body["tools"])
	}
	text := body["system"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Index(text, "- a") > strings.Index(text, "- z") {
		t.Fatalf("skills reminder was not sorted: %s", text)
	}
}

func TestStabilizeSessionRelocatesLatestBlocks(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "<system-reminder>\nThe following skills are available\n\n- z\n- a\n</system-reminder>", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "<system-reminder>\nThe following deferred tools are now available\ntool-z\ntool-a\n</system-reminder>"},
		}},
	}}
	changed, reason, anomaly := stabilizeSession(body)
	if !changed || anomaly != "" || reason == "" {
		t.Fatalf("stabilizeSession changed=%v reason=%q anomaly=%q", changed, reason, anomaly)
	}
	messages := body["messages"].([]any)
	first := messages[0].(map[string]any)["content"].([]any)
	if len(first) != 3 {
		t.Fatalf("relocated content = %#v", first)
	}
	if strings.Contains(first[0].(map[string]any)["text"].(string), "deferred tools") == false || strings.Contains(first[1].(map[string]any)["text"].(string), "skills are available") == false {
		t.Fatalf("relocation order = %#v", first)
	}
	second := messages[1].(map[string]any)["content"].([]any)
	if len(second) != 0 {
		t.Fatalf("scattered blocks remained: %#v", second)
	}
	if _, exists := first[0].(map[string]any)["cache_control"]; exists {
		t.Fatal("relocated block retained cache_control")
	}
}

func TestNormalizeIdentityRemovesVolatileSessionText(t *testing.T) {
	body := map[string]any{
		"system":   []any{map[string]any{"type": "text", "text": "SessionStart:resume hook success:\n<session-id>abc</session-id>\nLast active: yesterday\n<session_knowledge>volatile</session_knowledge>"}},
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "SessionStart:resume hook success:"}}}},
	}
	changed, _, anomaly := normalizeIdentity(body)
	if !changed || anomaly != "" {
		t.Fatalf("normalizeIdentity changed=%v anomaly=%q", changed, anomaly)
	}
	systemText := body["system"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(systemText, "resume hook") || strings.Contains(systemText, "session-id") || strings.Contains(systemText, "session_knowledge") || strings.Contains(systemText, "Last active") {
		t.Fatalf("volatile system text remained: %s", systemText)
	}
	messageText := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(messageText, "resume hook") {
		t.Fatalf("volatile message text remained: %s", messageText)
	}
}

func TestApplyAutoLeavesSDKUntouched(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"system":"x"}`)
	result, err := Apply(input, http.Header{"User-Agent": []string{"anthropic-sdk/1"}}, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if result.Detected || string(result.Body) != string(input) {
		t.Fatalf("SDK request changed: %+v body=%s", result, result.Body)
	}
}

func TestApplyOffPassesThroughMalformedBody(t *testing.T) {
	input := []byte(`{"messages":`)
	result, err := Apply(input, http.Header{"X-Claude-Code": []string{"true"}}, Options{Mode: ModeOff})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != string(input) {
		t.Fatalf("disabled cache repair changed body: got %q want %q", result.Body, input)
	}
	if result.Detected || result.Enabled || result.Changed || len(result.Transforms) == 0 {
		t.Fatalf("disabled cache repair reported activity: %+v", result)
	}
	for _, transform := range result.Transforms {
		if !transform.Skipped || transform.Reason != "disabled" {
			t.Fatalf("transform was not marked disabled: %+v", transform)
		}
	}
}

func TestExtractUsage(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":60,"cache_creation_input_tokens":20,"reasoning_tokens":3}}`))
	if u.UncachedInputTokens != 20 || u.HitRatio != 0.6 || u.CreationRatio != 0.2 || u.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestExtractUsageAcceptsCamelCaseAndNestedTokenDetails(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"inputTokens":100,"outputTokens":12,"promptTokensDetails":{"cachedTokens":60},"completionTokensDetails":{"reasoningTokens":4}}}`))
	if u.InputTokens != 100 || u.OutputTokens != 12 || u.CacheReadInputTokens != 60 || u.ReasoningTokens != 4 || u.UncachedInputTokens != 40 {
		t.Fatalf("camelCase/nested usage=%+v", u)
	}
	if u.HitRatio != 0.6 || u.CreationRatio != 0 {
		t.Fatalf("cache ratios=%v/%v, want 0.6/0", u.HitRatio, u.CreationRatio)
	}
}

func TestExtractUsagePrefersExplicitUncachedTokens(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"input_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"uncached_input_tokens":7}}`))
	if u.UncachedInputTokens != 7 {
		t.Fatalf("uncached input = %d, want explicit value 7", u.UncachedInputTokens)
	}
	if u.HitRatio != 30.0/47.0 || u.CreationRatio != 10.0/47.0 {
		t.Fatalf("cache ratios = %v/%v, want %v/%v", u.HitRatio, u.CreationRatio, 30.0/47.0, 10.0/47.0)
	}
}

func TestExtractUsageFromSSE(t *testing.T) {
	body := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":30,\"cache_creation_input_tokens\":10}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"data: [DONE]\n\n")
	u := ExtractUsage(body)
	if u.InputTokens != 20 || u.OutputTokens != 7 || u.CacheReadInputTokens != 30 || u.CacheCreationInputTokens != 10 || u.UncachedInputTokens != 0 {
		t.Fatalf("SSE usage = %+v", u)
	}
	if u.HitRatio != 30.0/40.0 || u.CreationRatio != 10.0/40.0 {
		t.Fatalf("SSE cache ratios = %v/%v, want %v/%v", u.HitRatio, u.CreationRatio, 30.0/40.0, 10.0/40.0)
	}
}

// vLLM with --enable-force-include-usage repeats a cumulative usage snapshot
// on every chunk, but only the final chunk carries prompt_tokens_details.
// The uncached partition derived from an intermediate snapshot (the full
// prompt) must not survive the merge with the final cache read.
func TestExtractUsageFromVLLMForcedUsageSSE(t *testing.T) {
	body := []byte("data: {\"usage\":{\"prompt_tokens\":100,\"total_tokens\":100,\"completion_tokens\":0}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"total_tokens\":110,\"completion_tokens\":10}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"total_tokens\":130,\"completion_tokens\":30,\"prompt_tokens_details\":{\"cached_tokens\":70}}}\n\n" +
		"data: [DONE]\n\n")
	u := ExtractUsage(body)
	if u.InputTokens != 100 || u.OutputTokens != 30 || u.CacheReadInputTokens != 70 {
		t.Fatalf("vLLM SSE usage = %+v", u)
	}
	if u.UncachedInputTokens != 30 {
		t.Fatalf("vLLM SSE uncached = %d, want derived 30", u.UncachedInputTokens)
	}
	if u.HitRatio != 0.7 || u.CreationRatio != 0 {
		t.Fatalf("vLLM SSE cache ratios = %v/%v, want 0.7/0", u.HitRatio, u.CreationRatio)
	}
}

func TestExtractUsageHonorsExplicitZeroUncachedPartition(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"input_tokens":20,"cache_read_input_tokens":20,"uncached_input_tokens":0}}`))
	if u.UncachedInputTokens != 0 || u.HitRatio != 1 || u.CreationRatio != 0 {
		t.Fatalf("usage = %+v, want zero uncached and 100%% hit", u)
	}
}

func TestExtractUsageClampsMalformedCountersAndParsesNumericStrings(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"input_tokens":"20.0","output_tokens":"-3","cache_read_input_tokens":"-4","cache_creation_input_tokens":"7","reasoning_tokens":"9"}}`))
	if u.InputTokens != 20 || u.OutputTokens != 0 || u.CacheReadInputTokens != 0 || u.CacheCreationInputTokens != 7 || u.UncachedInputTokens != 13 {
		t.Fatalf("usage = %+v", u)
	}
	if u.HitRatio != 0 || u.CreationRatio != 7.0/20.0 {
		t.Fatalf("cache ratios = %v/%v, want 0/%v", u.HitRatio, u.CreationRatio, 7.0/20.0)
	}
}

func TestExtractUsageRejectsOverflowAndBoundsHitRatio(t *testing.T) {
	u := ExtractUsage([]byte(`{"usage":{"input_tokens":1e100,"cache_read_input_tokens":1e100,"cache_creation_input_tokens":-1e100,"uncached_input_tokens":-2}}`))
	if u.InputTokens != 0 || u.CacheReadInputTokens != 0 || u.CacheCreationInputTokens != 0 || u.UncachedInputTokens != 0 || u.HitRatio != 0 {
		t.Fatalf("overflow usage = %+v", u)
	}

	u = ExtractUsage([]byte(`{"usage":{"cache_read_input_tokens":"9223372036854775807","uncached_input_tokens":"1"}}`))
	if u.HitRatio < 0 || u.HitRatio > 1 || u.CacheReadInputTokens != math.MaxInt64 {
		t.Fatalf("bounded usage = %+v", u)
	}
}

func TestSessionUsageAccumulatesBySession(t *testing.T) {
	var sessions SessionUsage
	first := sessions.Add("session-1", Usage{CacheReadInputTokens: 30, CacheCreationInputTokens: 10, UncachedInputTokens: 20})
	if first.HitRatio != 0.5 || first.CreationRatio != 10.0/60.0 {
		t.Fatalf("first cache ratios = %v/%v, want 0.5/%v", first.HitRatio, first.CreationRatio, 10.0/60.0)
	}
	second := sessions.Add("session-1", Usage{CacheReadInputTokens: 10, UncachedInputTokens: 30})
	if second.CacheReadInputTokens != 40 || second.UncachedInputTokens != 50 || second.HitRatio != 40.0/100.0 || second.CreationRatio != 10.0/100.0 {
		t.Fatalf("cumulative usage = %+v", second)
	}
	other := sessions.Add("session-2", Usage{CacheReadInputTokens: 1, UncachedInputTokens: 3})
	if other.HitRatio != 0.25 || other.CreationRatio != 0 {
		t.Fatalf("other session cache ratios = %v/%v, want 0.25/0", other.HitRatio, other.CreationRatio)
	}
}

func TestSessionUsageClampsNegativeAndSaturatesCounters(t *testing.T) {
	var sessions SessionUsage
	first := sessions.Add("session-1", Usage{CacheReadInputTokens: -10, UncachedInputTokens: 4})
	if first.CacheReadInputTokens != 0 || first.UncachedInputTokens != 4 || first.HitRatio != 0 {
		t.Fatalf("negative usage = %+v", first)
	}
	second := sessions.Add("session-1", Usage{CacheReadInputTokens: math.MaxInt64, UncachedInputTokens: math.MaxInt64})
	if second.CacheReadInputTokens != math.MaxInt64 || second.UncachedInputTokens != math.MaxInt64 || second.HitRatio < 0 || second.HitRatio > 1 {
		t.Fatalf("saturated usage = %+v", second)
	}
}

func TestSessionUsageBoundsCardinalityAndRetainsActiveSessions(t *testing.T) {
	sessions := NewSessionUsage(2)
	sessions.Add("session-1", Usage{CacheReadInputTokens: 3})
	sessions.Add("session-2", Usage{CacheReadInputTokens: 5})
	// Touch session-1 so session-2 becomes the eviction candidate.
	if got := sessions.Add("session-1", Usage{UncachedInputTokens: 2}); got.CacheReadInputTokens != 3 || got.UncachedInputTokens != 2 {
		t.Fatalf("active session usage = %+v", got)
	}
	sessions.Add("session-3", Usage{CacheReadInputTokens: 7})
	if got := sessions.Count(); got != 2 {
		t.Fatalf("retained session count = %d, want 2", got)
	}
	// Keep session-1 hot before bringing the evicted session back. The evicted
	// session starts a fresh aggregate instead of resurrecting stale counters.
	if got := sessions.Add("session-1", Usage{}); got.CacheReadInputTokens != 3 || got.UncachedInputTokens != 2 {
		t.Fatalf("retained session usage = %+v", got)
	}
	if got := sessions.Add("session-2", Usage{UncachedInputTokens: 1}); got.CacheReadInputTokens != 0 || got.UncachedInputTokens != 1 {
		t.Fatalf("evicted session usage = %+v", got)
	}
}

func TestSessionUsageZeroValueUsesDefaultBound(t *testing.T) {
	var sessions SessionUsage
	for i := 0; i < DefaultMaxSessionEntries+1; i++ {
		sessions.Add("session-"+strconv.Itoa(i), Usage{UncachedInputTokens: 1})
	}
	if got := sessions.Count(); got != DefaultMaxSessionEntries {
		t.Fatalf("zero-value retained session count = %d, want %d", got, DefaultMaxSessionEntries)
	}
}

func TestSessionUsageFoldsOversizedSessionIDs(t *testing.T) {
	usage := NewSessionUsage(2)
	longID := strings.Repeat("x", maxSessionIDBytes+1024)
	usage.Add(longID, Usage{CacheReadInputTokens: 4})
	if got := usage.Add(longID, Usage{CacheReadInputTokens: 3}); got.CacheReadInputTokens != 7 {
		t.Fatalf("oversized session id did not retain cumulative usage: %+v", got)
	}
	usage.Add("normal", Usage{CacheCreationInputTokens: 2})
	if usage.Count() != 2 {
		t.Fatalf("session count=%d, want 2", usage.Count())
	}
}
