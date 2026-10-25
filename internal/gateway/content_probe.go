package gateway

import (
	"bytes"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// probeVerdict is what the first bytes of a success response say about the
// answer it carries.
//
// The gateway only ever writes a response to the client once it knows there is
// something to show. An upstream that answers 200 with a stream of lifecycle
// frames and then stops without producing a single token is not answering the
// request at all; committing it would hand the caller a successful-looking
// empty result and break whatever runs downstream of it.
type probeVerdict int

const (
	// probePending: not enough information yet, keep buffering.
	probePending probeVerdict = iota
	// probeContent: the response carries model output, commit it.
	probeContent
	// probeEmpty: the response completed without any model output.
	probeEmpty
)

// contentProbe inspects a success response as it arrives and classifies the
// first meaningful output. It understands the four wire dialects the gateway
// forwards, because "has content" is dialect-specific: a Chat stream signals
// text in choices[].delta, a Responses stream in response.output_text.delta,
// and so on. Counting bytes or looking for a generic key would both misread
// lifecycle frames as content and miss tool-call-only replies.
//
// The probe never decides on an unknown shape: it reports probePending so the
// caller fails open and forwards the response unchanged. Correctly retrying a
// broken provider is worth a lot, but inventing a retry for a legitimate
// answer is worse than the bug being fixed.
type contentProbe struct {
	dialect   string
	sse       bool
	pending   []byte
	body      []byte
	truncated bool
	// sawBytes records that any body arrived at all. A 200 with no body is an
	// empty answer; a body this probe could not read is not.
	sawBytes bool

	hasContent bool
	terminal   bool
	// recognized records that at least one chunk matched a known dialect
	// shape. Without it, "no content" cannot be distinguished from "not a
	// response this probe understands", and only the former may be retried.
	recognized bool
	// unknown records a chunk that is definitely not this probe's dialect
	// (unparsable payload, or a shape no dialect produces). It ends inspection
	// immediately: holding a response we cannot read would delay a legitimate
	// stream for the whole grace period.
	unknown bool
	// acknowledged records a response that correctly answers the request
	// without carrying model output: a provider-owned background submit
	// answers status=queued/in_progress before any token exists. It is not
	// empty (nothing to retry) and not content, so it must be forwarded at
	// once instead of being held for the first-token grace period.
	acknowledged bool
}

func newContentProbe(dialect string, sse bool) *contentProbe {
	return &contentProbe{dialect: dialect, sse: sse}
}

// Write feeds one response chunk to the probe.
func (p *contentProbe) Write(chunk []byte) {
	if p == nil || len(chunk) == 0 || p.hasContent {
		return
	}
	p.sawBytes = true
	if p.sse {
		p.pending = append(p.pending, chunk...)
		for {
			newline := bytes.IndexByte(p.pending, '\n')
			if newline < 0 {
				break
			}
			line := bytes.TrimSpace(p.pending[:newline])
			p.pending = p.pending[newline+1:]
			p.consumeSSELine(line)
		}
		// A gateway can flush the final data line without the trailing
		// newline; parse it once its JSON is complete.
		if tail := bytes.TrimSpace(p.pending); len(tail) > 0 && bytes.HasPrefix(tail, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(tail, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) || gjson.ValidBytes(payload) {
				p.consumeSSELine(tail)
				p.pending = nil
			}
		}
		// Bound the partial line: a provider that never sends a newline must
		// not make this buffer grow without limit.
		if len(p.pending) > probeMaxLineBytes {
			p.pending = p.pending[len(p.pending)-probeMaxLineBytes:]
		}
		return
	}

	if p.truncated {
		return
	}
	remaining := probeMaxBodyBytes - len(p.body)
	if remaining <= 0 {
		p.truncated = true
		return
	}
	if len(chunk) > remaining {
		p.body = append(p.body, chunk[:remaining]...)
		p.truncated = true
		return
	}
	p.body = append(p.body, chunk...)
	// A whole JSON object that has fully arrived can be judged without waiting
	// for the connection to close. Without this, a chunked body (no declared
	// length) stays unclassified for the entire first-token grace period even
	// when it was complete after one read: a background acknowledgement would
	// be held for nothing, and an empty answer would sit there instead of
	// retrying. The cheap "starts with { and ends with }" gate keeps a large
	// base64 or array body from being re-parsed on every chunk, and the size
	// bound keeps the check linear: an answer this large is not an
	// acknowledgement, so Finish can judge it once.
	if body := bytes.TrimSpace(p.body); len(body) > 1 && len(body) <= probeMaxIncrementalBytes &&
		body[0] == '{' && body[len(body)-1] == '}' && gjson.ValidBytes(body) {
		p.classify(body)
	}
}

// Finish closes the probe at end of body.
func (p *contentProbe) Finish() {
	if p == nil || p.hasContent {
		return
	}
	if p.sse {
		switch {
		case p.recognized:
			// A stream that ended is terminal by definition, even without an
			// explicit completion event: the upstream closed the connection.
			p.terminal = true
		case p.sawBytes:
			// An upstream may declare text/event-stream and still answer with
			// one ordinary JSON body (an ignored alt=sse). Nothing here matched
			// an SSE frame, so "no content" cannot be told apart from "not a
			// stream at all": forward the body instead of retrying a legitimate
			// answer.
			p.unknown = true
		default:
			// A 200 with no body at all is an empty answer.
			p.terminal = true
		}
		return
	}
	if p.truncated {
		return
	}
	body := bytes.TrimSpace(p.body)
	if len(body) == 0 {
		p.terminal = true
		return
	}
	if !gjson.ValidBytes(body) {
		// A partial or non-JSON body: not a shape we can judge.
		p.unknown = true
		return
	}
	// Only a body this probe actually understood may be called empty. A
	// success body in some other shape (an image job, a custom protocol, a
	// test double) is unrecognized, not empty: forwarding it unchanged is
	// right, retrying it would replace a real answer with a duplicate request.
	p.classify(body)
	if !p.recognized {
		return
	}
	if p.acknowledged {
		// A job acknowledgement, not an answer: nothing to retry and nothing
		// to wait for.
		return
	}
	p.terminal = true
}

// Forward reports whether the response must be handed over immediately
// without waiting for content or judging it empty.
func (p *contentProbe) Forward() bool {
	return p != nil && p.acknowledged
}

// Verdict reports what the probe has learned so far.
func (p *contentProbe) Verdict() probeVerdict {
	if p == nil {
		return probePending
	}
	if p.hasContent {
		return probeContent
	}
	if p.terminal {
		return probeEmpty
	}
	return probePending
}

func (p *contentProbe) consumeSSELine(line []byte) {
	if len(line) == 0 || line[0] == ':' {
		// Blank separator or a comment/keepalive line.
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		// event:/id:/retry: carry no payload.
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 {
		return
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		// The Chat dialect's end sentinel: this is a chat stream, and it is
		// over. A chat stream that produced nothing before it is empty.
		p.recognized = true
		p.terminal = true
		return
	}
	if !gjson.ValidBytes(payload) {
		p.unknown = true
		return
	}
	before := p.recognized
	p.classify(payload)
	if !p.recognized && !before {
		p.unknown = true
	}
}

// Unknown reports whether the response turned out not to be a shape this
// probe understands. Such a response must be forwarded at once.
func (p *contentProbe) Unknown() bool {
	return p != nil && p.unknown
}

func (p *contentProbe) classify(payload []byte) {
	doc := gjson.ParseBytes(payload)
	eventType := firstProbeString(doc, "event_type", "type")
	switch {
	case strings.HasPrefix(eventType, "response."):
		p.classifyResponses(doc)
		return
	case isKnownInteractionsEvent(eventType):
		p.classifyInteractions(doc)
		return
	case eventType != "error" && isKnownAnthropicEvent(eventType):
		p.classifyAnthropic(doc)
		return
	}
	switch p.dialect {
	case config.ProviderTypeOpenAI:
		p.classifyChat(doc)
	case config.ProviderTypeOpenAIResponses:
		p.classifyResponses(doc)
	case config.ProviderTypeAnthropic:
		p.classifyAnthropic(doc)
	case config.ProviderTypeGemini:
		p.classifyGemini(doc)
	case config.ProviderTypeGeminiInteractions:
		p.classifyInteractions(doc)
	}
}

// classifyChat understands both stream deltas and whole completion bodies.
func (p *contentProbe) classifyChat(doc gjson.Result) {
	if doc.Get("error").Exists() {
		p.terminal = true
		return
	}
	choices := doc.Get("choices")
	if !choices.IsArray() {
		return
	}
	p.recognized = true
	choices.ForEach(func(_, choice gjson.Result) bool {
		if chatChoiceHasContent(choice) {
			p.hasContent = true
			return false
		}
		if reason := choice.Get("finish_reason"); reason.Exists() && reason.String() != "" {
			p.terminal = true
		}
		return true
	})
}

func chatChoiceHasContent(choice gjson.Result) bool {
	for _, key := range []string{"delta", "message"} {
		container := choice.Get(key)
		if !container.Exists() {
			continue
		}
		if contentValueHasText(container.Get("content")) ||
			contentValueHasText(container.Get("reasoning_content")) ||
			contentValueHasText(container.Get("reasoning")) ||
			contentValueHasText(container.Get("refusal")) {
			return true
		}
		if calls := container.Get("tool_calls"); calls.IsArray() && len(calls.Array()) > 0 {
			return true
		}
		if container.Get("function_call").Exists() {
			return true
		}
	}
	return contentValueHasText(choice.Get("text"))
}

// classifyResponses understands Responses stream events and whole bodies.
func (p *contentProbe) classifyResponses(doc gjson.Result) {
	eventType := doc.Get("type").String()
	if isKnownResponsesEvent(eventType) {
		p.recognized = true
	}
	switch eventType {
	case "response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_text.delta", "response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		if doc.Get("delta").String() != "" {
			p.hasContent = true
		}
	case "response.output_item.added", "response.output_item.done":
		if responsesItemHasContent(doc.Get("item")) {
			p.hasContent = true
		}
	case "response.content_part.added", "response.content_part.done":
		if contentValueHasText(doc.Get("part.text")) {
			p.hasContent = true
		}
	case "response.completed":
		p.terminal = true
		if responsesOutputHasContent(doc.Get("response.output")) {
			p.hasContent = true
		}
	case "response.incomplete", "response.failed", "error":
		p.terminal = true
	case "response.created", "response.in_progress", "response.queued",
		"response.output_text.done", "response.content_part", "response.output_item":
		// Lifecycle only: nothing to decide.
	default:
		p.classifyResponsesBody(doc)
	}
}

func (p *contentProbe) classifyResponsesBody(doc gjson.Result) {
	if doc.Get("object").String() == "response" || doc.Get("output").IsArray() {
		p.recognized = true
	}
	if doc.Get("error").Exists() {
		p.terminal = true
	}
	if responsesOutputHasContent(doc.Get("output")) {
		p.hasContent = true
	}
	switch doc.Get("status").String() {
	case "completed", "incomplete", "failed":
		p.terminal = true
	case "queued", "in_progress", "running":
		// Only a background submit answers with a lifecycle status and no
		// output. Forward it: the caller polls the job itself.
		p.acknowledged = true
	}
}

func responsesOutputHasContent(output gjson.Result) bool {
	found := false
	output.ForEach(func(_, item gjson.Result) bool {
		if responsesItemHasContent(item) {
			found = true
			return false
		}
		return true
	})
	return found
}

func responsesItemHasContent(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch item.Get("type").String() {
	case "function_call", "custom_tool_call", "local_shell_call", "mcp_call", "mcp_approval_request", "mcp_list_tools", "web_search_call":
		return true
	case "image_generation_call":
		return item.Get("result").String() != ""
	}
	if contentValueHasText(item.Get("text")) || contentValueHasText(item.Get("refusal")) {
		return true
	}
	content := item.Get("content")
	if content.IsArray() {
		for _, part := range content.Array() {
			if contentValueHasText(part.Get("text")) || contentValueHasText(part.Get("refusal")) {
				return true
			}
		}
	}
	// Reasoning items stream their summary text separately.
	if item.Get("type").String() == "reasoning" && item.Get("summary").IsArray() && len(item.Get("summary").Array()) > 0 {
		return true
	}
	return false
}

// classifyAnthropic understands Messages stream events and whole bodies.
func (p *contentProbe) classifyAnthropic(doc gjson.Result) {
	eventType := doc.Get("type").String()
	if isKnownAnthropicEvent(eventType) {
		p.recognized = true
	}
	switch eventType {
	case "content_block_start":
		block := doc.Get("content_block")
		switch block.Get("type").String() {
		case "tool_use", "server_tool_use", "mcp_tool_use":
			p.hasContent = true
		}
		if contentValueHasText(block.Get("text")) || contentValueHasText(block.Get("thinking")) {
			p.hasContent = true
		}
	case "content_block_delta":
		delta := doc.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			if delta.Get("text").String() != "" {
				p.hasContent = true
			}
		case "thinking_delta":
			if delta.Get("thinking").String() != "" {
				p.hasContent = true
			}
		case "input_json_delta":
			if delta.Get("partial_json").String() != "" {
				p.hasContent = true
			}
		}
	case "message_delta":
		if doc.Get("delta.stop_reason").Exists() && doc.Get("delta.stop_reason").String() != "" {
			p.terminal = true
		}
	case "message_stop":
		p.terminal = true
	case "error":
		p.terminal = true
	case "message_start", "ping", "content_block_stop":
		// Lifecycle only.
	case "message":
		// Whole (non-streaming) body.
		if anthropicContentHasContent(doc.Get("content")) {
			p.hasContent = true
		}
		if doc.Get("error").Exists() {
			p.terminal = true
		}
		if doc.Get("stop_reason").Exists() && doc.Get("stop_reason").String() != "" {
			p.terminal = true
		}
	}
}

func anthropicContentHasContent(content gjson.Result) bool {
	found := false
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_use", "server_tool_use", "mcp_tool_use":
			found = true
			return false
		}
		if contentValueHasText(block.Get("text")) || contentValueHasText(block.Get("thinking")) {
			found = true
			return false
		}
		return true
	})
	return found
}

// classifyGemini understands generateContent responses, streaming or not: the
// frames carry the same shape.
func (p *contentProbe) classifyGemini(doc gjson.Result) {
	if doc.Get("error").Exists() {
		p.terminal = true
		return
	}
	if reason := doc.Get("promptFeedback.blockReason"); reason.Exists() && reason.String() != "" {
		p.terminal = true
	}
	candidates := doc.Get("candidates")
	if candidates.IsArray() || doc.Get("promptFeedback").Exists() {
		p.recognized = true
	}
	if !candidates.IsArray() {
		return
	}
	candidates.ForEach(func(_, candidate gjson.Result) bool {
		parts := candidate.Get("content.parts")
		if parts.IsArray() {
			for _, part := range parts.Array() {
				if contentValueHasText(part.Get("text")) || part.Get("functionCall").Exists() {
					p.hasContent = true
					return false
				}
			}
		}
		if reason := candidate.Get("finishReason"); reason.Exists() && reason.String() != "" {
			p.terminal = true
		}
		return true
	})
}

// classifyInteractions understands Gemini Interactions streams.
func (p *contentProbe) classifyInteractions(doc gjson.Result) {
	eventType := firstProbeString(doc, "event_type", "type")
	if isKnownInteractionsEvent(eventType) {
		p.recognized = true
	}
	switch eventType {
	case "step.delta":
		delta := doc.Get("delta")
		switch firstProbeString(delta, "type", "delta_type") {
		case "text", "thought_summary":
			if contentValueHasText(delta.Get("text")) || contentValueHasText(delta.Get("content")) {
				p.hasContent = true
			}
		case "arguments", "arguments_delta":
			if contentValueHasText(delta.Get("arguments")) || contentValueHasText(delta.Get("partial_json")) {
				p.hasContent = true
			}
		}
	case "step.start":
		if firstProbeString(doc, "step.type", "step_type", "type_name") == "function_call" {
			p.hasContent = true
		}
	case "interaction.completed", "error":
		p.terminal = true
	case "interaction.created", "interaction.in_progress", "step.stop":
		// Lifecycle only.
	default:
		if steps := doc.Get("steps"); steps.IsArray() {
			p.recognized = true
			for _, step := range steps.Array() {
				if step.Get("type").String() == "function_call" || contentValueHasText(step.Get("content")) || contentValueHasText(step.Get("summary")) {
					p.hasContent = true
				}
			}
			switch doc.Get("status").String() {
			case "queued", "in_progress", "running":
				// A background interaction acknowledgement: forward it, the
				// caller polls the interaction itself.
				p.acknowledged = true
			}
		}
	}
}

func firstProbeString(value gjson.Result, keys ...string) string {
	for _, key := range keys {
		if field := value.Get(key); field.Exists() && field.String() != "" {
			return field.String()
		}
	}
	return ""
}

// contentValueHasText reports whether a content-bearing JSON value carries any
// text. It accepts the string form and the array-of-parts form, because
// providers use both for the same field.
func contentValueHasText(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	switch {
	case value.Type == gjson.String:
		return value.String() != ""
	case value.IsArray():
		for _, item := range value.Array() {
			if item.Type == gjson.String && item.String() != "" {
				return true
			}
			if contentValueHasText(item.Get("text")) {
				return true
			}
		}
	case value.IsObject():
		return contentValueHasText(value.Get("text"))
	}
	return false
}

// HasContent reports whether the probe has already seen model output.
func (p *contentProbe) HasContent() bool {
	return p != nil && p.hasContent
}

const (
	// probeMaxBodyBytes bounds the whole-body buffer used for non-streaming
	// responses. A JSON body larger than this is not a chat answer we need to
	// parse (an inline image, for instance), so the probe fails open.
	probeMaxBodyBytes = 8 << 20
	// probeMaxLineBytes bounds one unterminated SSE line.
	probeMaxLineBytes = 256 << 10
	// probeMaxIncrementalBytes bounds the body an incremental classification
	// re-parses on every chunk. Larger bodies are judged once, at end of body.
	probeMaxIncrementalBytes = 64 << 10
)

// emptyResponsePolicy bounds the "model answered nothing" retry.
type emptyResponsePolicy struct {
	enabled bool
	hold    time.Duration
}

func resolveEmptyResponse(cfg config.GatewayEmptyResponseConfig) emptyResponsePolicy {
	return emptyResponsePolicy{
		enabled: cfg.RetryEnabled(),
		hold:    cfg.EffectiveHold(),
	}
}

// emptyResponseCommitBufferBytes bounds how much content-free stream data is
// buffered while waiting for the first token. A response that produced this
// much without any recognizable output is not a shape the probe understands,
// so it is forwarded rather than retried.
const emptyResponseCommitBufferBytes = 2 << 20

// isKnownResponsesEvent reports whether a stream event type belongs to the
// Responses API. The set exists so "no content" can be told apart from "not a
// response at all".
func isKnownResponsesEvent(eventType string) bool {
	if eventType == "" {
		return false
	}
	return strings.HasPrefix(eventType, "response.") || eventType == "error"
}

// isKnownAnthropicEvent reports whether a stream event type belongs to the
// Anthropic Messages API.
func isKnownAnthropicEvent(eventType string) bool {
	switch eventType {
	case "message_start", "message_delta", "message_stop", "content_block_start",
		"content_block_delta", "content_block_stop", "ping", "error", "message":
		return true
	}
	return false
}

// isKnownInteractionsEvent reports whether a stream event type belongs to the
// Gemini Interactions API.
func isKnownInteractionsEvent(eventType string) bool {
	if eventType == "" {
		return false
	}
	return strings.HasPrefix(eventType, "step.") || strings.HasPrefix(eventType, "interaction.")
}
