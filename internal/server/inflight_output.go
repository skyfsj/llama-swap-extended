package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

const (
	// The preview is for an operator watching a live request, not a second
	// response buffer. Keeping only the latest 512 runes makes the event safe
	// for long generations and keeps the browser row readable.
	inflightOutputPreviewRunes = 512
	inflightOutputBufferBytes  = 16 * 1024
)

// inflightTokenTelemetry contains only counters and timings explicitly sent by
// the upstream. In particular, it never treats the number of bytes or runes in
// OutputPreview as a token count: tokenization is backend/model specific.
type inflightTokenTelemetry struct {
	inputTokens        int64
	outputTokens       int64
	cachedTokens       int64
	promptPerSecond    float64
	tokensPerSecond    float64
	firstTokenMs       int64
	promptMs           float64
	decodeMs           float64
	hasInput           bool
	hasOutput          bool
	hasCached          bool
	hasPromptRate      bool
	hasTokenRate       bool
	hasFirstToken      bool
	hasPromptDuration  bool
	hasDecodeDuration  bool
	promptUsesUncached bool
}

// inflightOutputParser extracts text deltas from common OpenAI-compatible,
// Anthropic, and Responses API payloads. It is owned by one inflightRequest
// and called while the tracker mutex is held, so it does not need its own
// synchronization.
type inflightOutputParser struct {
	pending       []byte
	body          []byte
	bodyTruncated bool
	directParsed  bool
	preview       string
	telemetry     inflightTokenTelemetry
}

func (p *inflightOutputParser) Append(data []byte) string {
	if p == nil || len(data) == 0 {
		if p == nil {
			return ""
		}
		return p.preview
	}

	p.appendBody(data)
	p.pending = append(p.pending, data...)
	p.consumeCompleteLines()
	if len(p.pending) > inflightOutputBufferBytes {
		p.pending = p.pending[len(p.pending)-inflightOutputBufferBytes:]
	}
	p.consumeCompleteTail()

	// Non-streaming JSON has no line boundary to trigger the SSE parser. Try
	// the bounded body after each write, but mark it parsed only once valid so
	// repeated writes cannot duplicate a response preview.
	if !p.directParsed && !p.bodyTruncated {
		body := bytes.TrimSpace(p.body)
		if len(body) > 0 && json.Valid(body) {
			p.appendJSON(body)
			p.directParsed = true
		}
	}
	return p.preview
}

func (p *inflightOutputParser) appendBody(data []byte) {
	if p.bodyTruncated {
		return
	}
	remaining := inflightOutputBufferBytes - len(p.body)
	if remaining <= 0 {
		p.bodyTruncated = true
		return
	}
	if len(data) > remaining {
		p.body = append(p.body, data[:remaining]...)
		p.bodyTruncated = true
		return
	}
	p.body = append(p.body, data...)
}

func (p *inflightOutputParser) consumeCompleteLines() {
	for {
		newline := bytes.IndexByte(p.pending, '\n')
		if newline < 0 {
			return
		}
		line := bytes.TrimSpace(p.pending[:newline])
		p.pending = p.pending[newline+1:]
		p.consumeLine(line)
	}
}

// Some small gateways flush a final SSE data line without the conventional
// trailing newline. Parse that line once its JSON becomes complete while
// retaining genuinely partial chunks for the next write.
func (p *inflightOutputParser) consumeCompleteTail() {
	line := bytes.TrimSpace(p.pending)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload) {
		p.consumeLine(line)
		p.pending = nil
	}
}

func (p *inflightOutputParser) consumeLine(line []byte) {
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
		return
	}
	p.appendJSON(payload)
}

func (p *inflightOutputParser) appendJSON(data []byte) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return
	}
	p.updateTelemetry(data)
	if event, ok := value.(map[string]any); ok {
		if eventType, ok := event["type"].(string); ok && strings.HasSuffix(eventType, ".delta") {
			if delta, ok := event["delta"].(string); ok {
				p.appendFragment(delta)
			}
		}
	}
	collectInflightOutput(value, "", &p.preview)
}

func (p *inflightOutputParser) updateTelemetry(data []byte) {
	if p == nil || !gjson.ValidBytes(data) {
		return
	}
	parsed := gjson.ParseBytes(data)
	for _, path := range usagePaths {
		usage := parsed.Get(path)
		if !usage.Exists() {
			continue
		}
		input, output, cached, ok := extractUsageTokens(usage)
		if !ok {
			continue
		}
		if usageHasAny(usage, "prompt_tokens", "promptTokens", "input_tokens", "inputTokens") {
			p.telemetry.inputTokens = input
			p.telemetry.hasInput = true
		}
		if usageHasAny(usage, "completion_tokens", "completionTokens", "output_tokens", "outputTokens") {
			p.telemetry.outputTokens = output
			p.telemetry.hasOutput = true
		}
		if cached >= 0 {
			p.telemetry.cachedTokens = cached
			p.telemetry.hasCached = true
		}
	}

	if timings := parsed.Get("timings"); timings.Exists() {
		if value, ok := firstMetricInt(timings, "prompt_n"); ok {
			p.telemetry.inputTokens = value
			p.telemetry.hasInput = true
		}
		if value, ok := firstMetricInt(timings, "predicted_n"); ok {
			p.telemetry.outputTokens = value
			p.telemetry.hasOutput = true
		}
		if value, ok := firstMetricInt(timings, "cache_n"); ok {
			p.telemetry.cachedTokens = value
			p.telemetry.hasCached = true
		}
		if value := metricRate(firstMetricFloat(timings, "prompt_per_second")); value >= 0 {
			p.telemetry.promptPerSecond = value
			p.telemetry.hasPromptRate = true
		}
		if value := metricRate(firstMetricFloat(timings, "predicted_per_second")); value >= 0 {
			p.telemetry.tokensPerSecond = value
			p.telemetry.hasTokenRate = true
		}
		if value := nonNegativeFinite(timings.Get("prompt_ms")); value > 0 {
			p.telemetry.promptMs = value
			p.telemetry.hasPromptDuration = true
		}
		if value := nonNegativeFinite(timings.Get("predicted_ms")); value > 0 {
			p.telemetry.decodeMs = value
			p.telemetry.hasDecodeDuration = true
		}
	}

	if metrics := parsed.Get("metrics"); metrics.Exists() {
		if value, ok := firstMetricInt(metrics, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens"); ok {
			p.telemetry.inputTokens = value
			p.telemetry.hasInput = true
		}
		if value, ok := firstMetricInt(metrics, "output_tokens", "outputTokens", "completion_tokens", "completionTokens"); ok {
			p.telemetry.outputTokens = value
			p.telemetry.hasOutput = true
		}
		if value, ok := firstMetricInt(metrics, "cached_tokens", "cachedTokens", "cache_tokens", "cacheTokens"); ok {
			p.telemetry.cachedTokens = value
			p.telemetry.hasCached = true
		}
		if value := metricRate(firstMetricFloat(metrics, "prompt_per_second", "prompt_tokens_per_second", "promptPerSecond", "promptTokensPerSecond")); value >= 0 {
			p.telemetry.promptPerSecond = value
			p.telemetry.hasPromptRate = true
		}
		if value := metricRate(firstMetricFloat(metrics, "tokens_per_second", "output_tokens_per_second", "tokensPerSecond", "outputTokensPerSecond")); value >= 0 {
			p.telemetry.tokensPerSecond = value
			p.telemetry.hasTokenRate = true
		}
		if value := firstMetricFloat(metrics, "prefill_time_ms", "prefillTimeMs"); value > 0 {
			p.telemetry.promptMs = value
			p.telemetry.hasPromptDuration = true
			p.telemetry.promptUsesUncached = true
		}
		if value := firstMetricFloat(metrics, "generation_time_ms", "generationTimeMs"); value > 0 {
			p.telemetry.decodeMs = value
			p.telemetry.hasDecodeDuration = true
		}
		if value := firstMetricFloat(metrics, "time_to_first_token_ms", "timeToFirstTokenMs", "first_token_ms", "firstTokenMs", "ttft_ms", "ttftMs"); value > 0 {
			p.telemetry.firstTokenMs = int64(metricMillis(value))
			p.telemetry.hasFirstToken = true
		}
	}
}

func (p *inflightOutputParser) Telemetry() inflightTokenTelemetry {
	if p == nil {
		return inflightTokenTelemetry{}
	}
	return p.telemetry
}

// collectInflightOutput walks only keys that carry response text. Restricting
// the walk avoids displaying ids, finish reasons, error messages, or tool
// metadata as if they were model output.
func collectInflightOutput(value any, key string, preview *string) {
	switch value := value.(type) {
	case string:
		if isInflightTextKey(key) {
			*preview = appendInflightPreview(*preview, value)
		}
	case []any:
		for _, item := range value {
			collectInflightOutput(item, key, preview)
		}
	case map[string]any:
		// JSON object order is not significant. Visit known fields in protocol
		// order so a content block and its reasoning text stay deterministic.
		for _, childKey := range []string{
			"choices", "output", "content", "message", "delta", "text",
			"output_text", "reasoning_content", "reasoning",
		} {
			child, ok := value[childKey]
			if !ok {
				continue
			}
			if isInflightTextKey(childKey) || isInflightContainerKey(childKey) {
				collectInflightOutput(child, childKey, preview)
			}
		}
	}
}

func isInflightTextKey(key string) bool {
	switch key {
	case "content", "text", "output_text", "reasoning_content", "reasoning":
		return true
	default:
		return false
	}
}

func isInflightContainerKey(key string) bool {
	switch key {
	case "choices", "output", "message", "delta":
		return true
	default:
		return false
	}
}

func (p *inflightOutputParser) appendFragment(fragment string) {
	p.preview = appendInflightPreview(p.preview, fragment)
}

func appendInflightPreview(current, fragment string) string {
	fragment = normalizeInflightOutput(fragment)
	if fragment == "" || strings.TrimSpace(fragment) == "" {
		return current
	}
	runes := []rune(current + fragment)
	if len(runes) <= inflightOutputPreviewRunes {
		return string(runes)
	}
	keep := inflightOutputPreviewRunes - 1
	return "…" + string(runes[len(runes)-keep:])
}

func normalizeInflightOutput(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
}
