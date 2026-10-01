package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSSEStateTooLarge is returned when a streaming Chat response would make
// the adapter retain more state than its bounded conversion budget. The
// client may already have received an earlier prefix of the stream; callers
// must therefore close the stream rather than attempting to append a second
// protocol error response or persist a partial affinity chain.
var ErrSSEStateTooLarge = errors.New("responses SSE state exceeds limit")

// Streaming conversion retains text/reasoning/tool arguments until the
// terminal lifecycle event. Keep that aggregate state bounded separately from
// the response-body capture limit: a client can receive an arbitrarily long
// stream, but the adapter must not accumulate it indefinitely in memory.
const maxSSEStateBytes = 64 << 20

// sseEvent frames one Responses SSE event with the standard event:/data:
// envelope the Responses client state machine expects.
func sseEvent(name string, data any) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		encoded = []byte("{}")
	}
	var out bytes.Buffer
	out.WriteString("event: ")
	out.WriteString(name)
	out.WriteString("\ndata: ")
	out.Write(encoded)
	out.WriteString("\n\n")
	return out.String()
}

func outputItemAddedEvent(outputIndex int, item map[string]any) string {
	return sseEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": outputIndex, "item": item,
	})
}

func outputItemDoneEvent(outputIndex int, item map[string]any) string {
	return sseEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIndex, "item": item,
	})
}

// stripSSEField reads a field value from an SSE line ("event: x", "data: y").
func stripSSEField(line, field string) (string, bool) {
	if !strings.HasPrefix(line, field+":") {
		return "", false
	}
	value := line[len(field)+1:]
	value = strings.TrimPrefix(value, " ")
	return value, true
}

type inlineThinkMode int

const (
	inlineThinkDetecting inlineThinkMode = iota
	inlineThinkReasoning
	inlineThinkText
)

type sseItemState struct {
	outputIndex int
	itemID      string
	text        strings.Builder
	added       bool
	done        bool
}

type sseToolCallState struct {
	outputIndex      int
	itemID           string
	callID           string
	name             string
	arguments        strings.Builder
	reasoningContent string
	added            bool
	done             bool
}

type indexedOutputItem struct {
	index int
	item  map[string]any
}

// SSEConverter converts a Chat Completions SSE byte stream into the Responses
// SSE lifecycle. It is a port of CC Switch's streaming_codex_chat converter
// and carries state between writes: a streaming caller feeds every upstream
// chunk to Convert and calls Flush once the upstream body has ended.
type SSEConverter struct {
	responseStarted bool
	completed       bool
	responseID      string
	model           string
	createdAt       uint64
	nextOutputIndex int

	text      sseItemState
	reasoning sseItemState

	inlineThink  inlineThinkMode
	inlineBuffer string

	tools              map[int]*sseToolCallState
	nextToolIndexToAdd int

	outputItems   []indexedOutputItem
	latestUsage   map[string]any
	hasUsage      bool
	finishReason  string
	toolContext   *ToolContext
	droppedCalls  int
	failedMessage string
	failedType    string
	pending       []byte
	blockLines    []string
	retainedBytes int
}

// NewSSEConverter creates a streaming converter without tool restoration
// context.
func NewSSEConverter() *SSEConverter {
	return NewSSEConverterWithContext(nil)
}

// NewSSEConverterWithContext creates a streaming converter that restores
// Responses tool vocabulary (namespace/custom/tool_search) using the tool
// context built from the original Responses request.
func NewSSEConverterWithContext(toolContext *ToolContext) *SSEConverter {
	return &SSEConverter{
		tools: map[int]*sseToolCallState{},
		// Deterministic placeholder; replaced by the upstream id or a
		// content-derived hash before the lifecycle opens.
		responseID:  "resp_" + shortSHA256Hex(nil),
		toolContext: toolContext,
	}
}

// Convert consumes the next chunk of upstream Chat SSE bytes and returns the
// converted Responses SSE bytes. It is intentionally line-oriented and
// accepts both LF and CRLF terminators.
func (c *SSEConverter) Convert(input []byte) ([]byte, error) {
	if c.completed {
		return nil, nil
	}
	if len(input) > maxSSEStateBytes || len(c.pending) > maxSSEStateBytes-len(input) {
		return nil, ErrSSEStateTooLarge
	}
	data := append(c.pending, input...)
	c.pending = nil
	var out bytes.Buffer
	for len(data) > 0 {
		lineEnd := bytes.IndexByte(data, '\n')
		if lineEnd < 0 {
			c.pending = append(c.pending, data...)
			break
		}
		line := strings.TrimSuffix(string(data[:lineEnd]), "\r")
		data = data[lineEnd+1:]
		if strings.TrimSpace(line) == "" {
			if len(c.blockLines) == 0 {
				continue
			}
			block := strings.Join(c.blockLines, "\n")
			c.blockLines = nil
			if c.processBlock(block, &out) {
				break
			}
			continue
		}
		c.blockLines = append(c.blockLines, line)
	}
	// Only long-lived retained state (text, reasoning, tool arguments,
	// pending buffers) counts against the budget; block lines above are
	// transient and released as soon as their event completes.
	if c.retainedBytes > maxSSEStateBytes {
		return out.Bytes(), ErrSSEStateTooLarge
	}
	return out.Bytes(), nil
}

// Flush processes a final unterminated SSE line and then applies the
// end-of-stream semantics of the CC Switch converter: a stream that ended
// with a finish reason finalizes normally, a stream with output but no
// finish reason is treated as truncated (incomplete/max_output_tokens), and
// an empty stream fails honestly instead of synthesizing a completed
// response.
func (c *SSEConverter) Flush() ([]byte, error) {
	var out bytes.Buffer
	if len(c.pending) > 0 {
		line := strings.TrimSuffix(string(c.pending), "\r")
		c.pending = nil
		if strings.TrimSpace(line) != "" {
			c.blockLines = append(c.blockLines, line)
		}
	}
	if len(c.blockLines) > 0 {
		block := strings.Join(c.blockLines, "\n")
		c.blockLines = nil
		c.processBlock(block, &out)
	}
	if !c.completed {
		switch {
		case c.finishReason != "":
			for _, event := range c.finalize() {
				out.WriteString(event)
			}
		case c.hasSubstantiveOutput():
			// The upstream truncated the stream mid-output. Synthesize the
			// same incomplete lifecycle a finish_reason=length turn would
			// produce instead of failing a mostly-successful response.
			c.finishReason = "length"
			for _, event := range c.finalize() {
				out.WriteString(event)
			}
		default:
			out.WriteString(c.failedEvent(
				"Upstream Chat Completions stream ended before sending finish_reason",
				"stream_truncated"))
		}
	}
	return out.Bytes(), nil
}

// processBlock handles one SSE block. It reports whether the stream has
// failed and no further blocks should be processed.
func (c *SSEConverter) processBlock(block string, out *bytes.Buffer) bool {
	eventName := ""
	var dataParts []string
	for _, line := range strings.Split(block, "\n") {
		if value, ok := stripSSEField(line, "event"); ok {
			eventName = strings.TrimSpace(value)
		}
		if value, ok := stripSSEField(line, "data"); ok {
			dataParts = append(dataParts, value)
		}
	}
	if len(dataParts) == 0 {
		return false
	}
	data := strings.Join(dataParts, "\n")
	if strings.TrimSpace(data) == "[DONE]" {
		for _, event := range c.finalize() {
			out.WriteString(event)
		}
		return false
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		// Not a Chat chunk (heartbeat, comment, gateway noise): skip it.
		return false
	}
	// The key's presence is not the signal. Providers that serialize a response
	// struct carrying an error field emit `"error": null` on every chunk, and
	// treating that as a failure turns a healthy stream into response.failed
	// with the message "null" after the first chunk.
	if errValue, hasError := chunk["error"]; eventName == "error" || (hasError && errValue != nil) {
		message, errorType := extractChatSSEError(chunk)
		out.WriteString(c.failedEvent(message, errorType))
		return true
	}
	for _, event := range c.handleChatChunk(chunk, []byte(data)) {
		out.WriteString(event)
	}
	return false
}

func extractChatSSEError(chunk map[string]any) (message, errorType string) {
	var errorValue any = chunk
	if nested, exists := chunk["error"]; exists {
		errorValue = nested
	}
	switch typed := errorValue.(type) {
	case string:
		message = typed
	case map[string]any:
		if text, ok := typed["message"].(string); ok {
			message = text
		} else if text, ok := typed["detail"].(string); ok {
			message = text
		} else {
			message = canonicalJSONString(typed)
		}
		if text, ok := typed["type"].(string); ok {
			errorType = text
		} else if text, ok := typed["code"].(string); ok {
			errorType = text
		}
	default:
		message = canonicalJSONString(errorValue)
	}
	return message, errorType
}

func (c *SSEConverter) handleChatChunk(chunk map[string]any, raw []byte) []string {
	if c.completed {
		return nil
	}
	var events []string
	if id, ok := chunk["id"].(string); ok {
		c.responseID = responseIDFromChatID(id)
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		c.model = model
	}
	if created, ok := jsonUint64Opt(chunk["created"]); ok {
		c.createdAt = created
	}

	if !c.responseStarted {
		if _, hasID := chunk["id"]; !hasID {
			// Keep the synthesized id deterministic for affinity dedup even
			// when the upstream never sends one.
			c.responseID = "resp_" + shortSHA256Hex(raw)
		}
		events = append(events, c.ensureResponseStarted()...)
	}

	if usage, exists := chunk["usage"]; exists && usage != nil {
		c.latestUsage = chatUsageToResponsesUsage(usage)
		c.hasUsage = true
	}

	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return events
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return events
	}

	if delta, ok := choice["delta"].(map[string]any); ok {
		if reasoning, ok := extractReasoningFieldText(delta); ok {
			events = append(events, c.pushReasoningDelta(reasoning)...)
			c.appendReasoningToActiveTools(reasoning)
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			events = append(events, c.pushContentDelta(content)...)
		}
		if toolCalls, ok := delta["tool_calls"].([]any); ok {
			events = append(events, c.flushInlineThinkAtBoundary()...)
			reasoningForToolCall := c.currentReasoningText()
			events = append(events, c.finalizeReasoning()...)
			for _, raw := range toolCalls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				events = append(events, c.pushToolCallDelta(call, reasoningForToolCall)...)
			}
		}
	}

	if finishReason, ok := choice["finish_reason"].(string); ok {
		c.finishReason = finishReason
	}
	return events
}

func (c *SSEConverter) ensureResponseStarted() []string {
	if c.responseStarted {
		return nil
	}
	c.responseStarted = true
	response := c.baseResponse("in_progress", []any{})
	return []string{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": response}),
		sseEvent("response.in_progress", map[string]any{"type": "response.in_progress", "response": response}),
	}
}

func (c *SSEConverter) pushReasoningDelta(delta string) []string {
	var events []string
	if !c.reasoning.added {
		c.reasoning.outputIndex = c.nextOutputIndex
		c.nextOutputIndex++
		c.reasoning.itemID = "rs_" + c.responseID
		c.reasoning.added = true
		events = append(events,
			outputItemAddedEvent(c.reasoning.outputIndex, map[string]any{
				"id": c.reasoning.itemID, "type": "reasoning", "status": "in_progress", "summary": []any{},
			}),
			sseEvent("response.reasoning_summary_part.added", map[string]any{
				"type": "response.reasoning_summary_part.added", "item_id": c.reasoning.itemID,
				"output_index": c.reasoning.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}))
	}
	c.noteRetained(len(delta))
	c.reasoning.text.WriteString(delta)
	events = append(events, sseEvent("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": c.reasoning.itemID,
		"output_index": c.reasoning.outputIndex, "summary_index": 0, "delta": delta,
	}))
	return events
}

func (c *SSEConverter) pushTextDelta(delta string) []string {
	var events []string
	if !c.text.added {
		c.text.outputIndex = c.nextOutputIndex
		c.nextOutputIndex++
		c.text.itemID = c.responseID + "_msg"
		c.text.added = true
		events = append(events,
			outputItemAddedEvent(c.text.outputIndex, map[string]any{
				"id": c.text.itemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			}),
			sseEvent("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": c.text.itemID,
				"output_index": c.text.outputIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}))
	}
	c.noteRetained(len(delta))
	c.text.text.WriteString(delta)
	events = append(events, sseEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": c.text.itemID,
		"output_index": c.text.outputIndex, "content_index": 0, "delta": delta,
	}))
	return events
}

func (c *SSEConverter) currentReasoningText() string {
	text := strings.TrimSpace(c.reasoning.text.String())
	return text
}

func (c *SSEConverter) pushContentDelta(delta string) []string {
	switch c.inlineThink {
	case inlineThinkText:
		events := c.finalizeReasoning()
		return append(events, c.pushTextDelta(delta)...)
	case inlineThinkDetecting:
		c.noteRetained(len(delta))
		c.inlineBuffer += delta
		switch leadingThinkPrefixDecision(c.inlineBuffer) {
		case thinkPrefixNeedMore:
			return nil
		case thinkPrefixReasoning:
			c.inlineThink = inlineThinkReasoning
			return c.drainCompleteInlineThink()
		default:
			c.inlineThink = inlineThinkText
			text := c.inlineBuffer
			c.inlineBuffer = ""
			events := c.finalizeReasoning()
			return append(events, c.pushTextDelta(text)...)
		}
	default:
		c.noteRetained(len(delta))
		c.inlineBuffer += delta
		return c.drainCompleteInlineThink()
	}
}

func (c *SSEConverter) drainCompleteInlineThink() []string {
	reasoning, answer, ok := splitLeadingThinkBlock(c.inlineBuffer)
	if !ok {
		return nil
	}
	c.inlineThink = inlineThinkText
	c.inlineBuffer = ""
	var events []string
	if reasoning != "" {
		events = append(events, c.pushReasoningDelta(reasoning)...)
		events = append(events, c.finalizeReasoning()...)
	}
	if answer != "" {
		events = append(events, c.pushTextDelta(answer)...)
	}
	return events
}

func (c *SSEConverter) flushInlineThinkAtBoundary() []string {
	switch c.inlineThink {
	case inlineThinkText:
		return nil
	case inlineThinkDetecting:
		c.inlineThink = inlineThinkText
		text := c.inlineBuffer
		c.inlineBuffer = ""
		if text == "" {
			return nil
		}
		events := c.finalizeReasoning()
		return append(events, c.pushTextDelta(text)...)
	default:
		buffered := c.inlineBuffer
		c.inlineBuffer = ""
		c.inlineThink = inlineThinkText
		if reasoning, answer, ok := splitLeadingThinkBlock(buffered); ok {
			var events []string
			if reasoning != "" {
				events = append(events, c.pushReasoningDelta(reasoning)...)
				events = append(events, c.finalizeReasoning()...)
			}
			if answer != "" {
				events = append(events, c.pushTextDelta(answer)...)
			}
			return events
		}
		reasoning, ok := stripLeadingThinkOpenTag(buffered)
		if !ok {
			reasoning = buffered
		}
		if reasoning == "" {
			return nil
		}
		events := c.pushReasoningDelta(reasoning)
		return append(events, c.finalizeReasoning()...)
	}
}

type thinkPrefixDecision int

const (
	thinkPrefixNeedMore thinkPrefixDecision = iota
	thinkPrefixReasoning
	thinkPrefixText
)

// leadingThinkPrefixDecision classifies buffered content while it is still
// impossible to tell whether a <think> block is starting: a full open tag
// means reasoning, a prefix of the open tag needs more bytes, anything else
// is answer text.
func leadingThinkPrefixDecision(buffer string) thinkPrefixDecision {
	trimmed := strings.TrimLeft(buffer, " \t\r\n")
	if trimmed == "" {
		return thinkPrefixNeedMore
	}
	if strings.HasPrefix(trimmed, thinkOpenTag) {
		return thinkPrefixReasoning
	}
	if strings.HasPrefix(thinkOpenTag, trimmed) {
		return thinkPrefixNeedMore
	}
	return thinkPrefixText
}

// resolveToolKeyWithoutIndex resolves the state key for a tool-call delta
// whose upstream omitted the (protocol-required) index field. A new key is
// allocated only when the delta proves it is a new call: a non-empty id that
// matches no known call. Everything else merges into the last known call —
// collapsing two parallel calls is recoverable, exploding one call's
// argument continuation into multiple items is not.
func (c *SSEConverter) resolveToolKeyWithoutIndex(toolCall map[string]any) int {
	lastKey := -1
	for key := range c.tools {
		if key > lastKey {
			lastKey = key
		}
	}
	id, ok := toolCall["id"].(string)
	if !ok || id == "" {
		if lastKey < 0 {
			return 0
		}
		return lastKey
	}
	for key, state := range c.tools {
		if state.callID == id {
			return key
		}
	}
	if lastKey < 0 {
		return 0
	}
	return lastKey + 1
}

func (c *SSEConverter) pushToolCallDelta(toolCall map[string]any, reasoning string) []string {
	chatIndex, hasIndex := jsonUint64Opt(toolCall["index"])
	if !hasIndex {
		chatIndex = uint64(c.resolveToolKeyWithoutIndex(toolCall))
	}
	key := int(chatIndex)

	idDelta, _ := toolCall["id"].(string)
	function, _ := toolCall["function"].(map[string]any)
	nameDelta, _ := function["name"].(string)
	argsDelta, _ := function["arguments"].(string)

	state, ok := c.tools[key]
	if !ok {
		state = &sseToolCallState{}
		c.tools[key] = state
	}
	if idDelta != "" {
		state.callID = idDelta
	}
	if nameDelta != "" {
		state.name = nameDelta
	}
	if argsDelta != "" {
		c.noteRetained(len(argsDelta))
		state.arguments.WriteString(argsDelta)
	}
	if state.reasoningContent == "" && strings.TrimSpace(reasoning) != "" {
		state.reasoningContent = strings.TrimSpace(reasoning)
	}

	isCustomTool := c.toolContext.IsCustomToolChatName(state.name)
	var events []string
	if argsDelta != "" && !isCustomTool && state.added {
		events = append(events, sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": state.itemID,
			"output_index": state.outputIndex, "delta": argsDelta,
		}))
	}
	events = append(events, c.flushReadyToolCalls()...)
	return events
}

// flushReadyToolCalls releases consecutive Chat tool indexes so late-arriving
// identity fragments cannot reorder parallel calls: a call is only announced
// to the client once its full id and name are known.
func (c *SSEConverter) flushReadyToolCalls() []string {
	var events []string
	for {
		key := c.nextToolIndexToAdd
		state, ok := c.tools[key]
		if !ok {
			break
		}
		if state.added || state.done {
			c.nextToolIndexToAdd++
			continue
		}
		if state.callID == "" || state.name == "" {
			break
		}
		assigned := c.nextOutputIndex
		c.nextOutputIndex++
		state.added = true
		state.outputIndex = assigned
		state.itemID = responseToolCallItemIDFromChatName(state.callID, state.name, c.toolContext)
		item := responseToolCallItemFromChatName(state.itemID, "in_progress", state.callID, state.name, "", state.reasoningContent, c.toolContext)
		events = append(events, outputItemAddedEvent(assigned, item))
		if state.arguments.Len() > 0 && !c.toolContext.IsCustomToolChatName(state.name) {
			events = append(events, sseEvent("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": state.itemID,
				"output_index": assigned, "delta": state.arguments.String(),
			}))
		}
		c.nextToolIndexToAdd++
	}
	return events
}

// appendReasoningToActiveTools attaches reasoning deltas to every tool call
// still being streamed: thinking models emit reasoning_content before the
// tool call, and Chat tool-call messages must carry it when replayed.
func (c *SSEConverter) appendReasoningToActiveTools(delta string) {
	if strings.TrimSpace(delta) == "" {
		return
	}
	for _, key := range c.sortedToolKeys() {
		state := c.tools[key]
		if state.done {
			continue
		}
		if state.reasoningContent == "" {
			state.reasoningContent = strings.TrimLeft(delta, " \t\r\n")
		} else {
			state.reasoningContent += delta
		}
	}
}

func (c *SSEConverter) sortedToolKeys() []int {
	keys := make([]int, 0, len(c.tools))
	for key := range c.tools {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func (c *SSEConverter) hasSubstantiveOutput() bool {
	if strings.TrimSpace(c.text.text.String()) != "" ||
		strings.TrimSpace(c.reasoning.text.String()) != "" ||
		strings.TrimSpace(c.inlineBuffer) != "" ||
		len(c.outputItems) > 0 {
		return true
	}
	for _, state := range c.tools {
		if state.added ||
			strings.TrimSpace(state.callID) != "" ||
			strings.TrimSpace(state.name) != "" ||
			strings.TrimSpace(state.arguments.String()) != "" ||
			strings.TrimSpace(state.reasoningContent) != "" {
			return true
		}
	}
	return false
}

// hasEmittedToolCall reports whether the final output contains at least one
// tool-call item the client can act on.
func (c *SSEConverter) hasEmittedToolCall() bool {
	for _, entry := range c.outputItems {
		switch itemType, _ := entry.item["type"].(string); itemType {
		case "function_call", "custom_tool_call", "tool_search_call":
			return true
		}
	}
	return false
}

func (c *SSEConverter) finalize() []string {
	if c.completed {
		return nil
	}
	var events []string
	events = append(events, c.ensureResponseStarted()...)
	events = append(events, c.flushInlineThinkAtBoundary()...)
	events = append(events, c.finalizeReasoning()...)
	events = append(events, c.finalizeText()...)
	events = append(events, c.finalizeTools()...)

	status := responseStatusFromFinishReason(c.finishReason)

	// When every streamed tool call was dropped for a missing name and
	// nothing else remains, the client would see a "completed" turn with no
	// tool call and silently end its agent loop. Report the failure honestly
	// instead. Only completed turns: finish_reason=length is a truncation,
	// and reporting it as a dropped tool call would misattribute the fault.
	if status == "completed" && c.droppedCalls > 0 && !c.hasEmittedToolCall() {
		message := fmt.Sprintf(
			"upstream returned %d tool call(s) without a function name, leaving no usable tool call in this turn",
			c.droppedCalls)
		return append(events, c.failedEvent(message, "upstream_tool_call_dropped"))
	}

	response := c.baseResponse(status, c.completedOutputItems())
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": responseIncompleteReason(c.finishReason)}
	}
	events = append(events, sseEvent("response.completed", map[string]any{
		"type": "response.completed", "response": response,
	}))
	c.completed = true
	return events
}

func (c *SSEConverter) finalizeReasoning() []string {
	if !c.reasoning.added || c.reasoning.done {
		return nil
	}
	text := c.reasoning.text.String()
	item := map[string]any{
		"id":   c.reasoning.itemID,
		"type": "reasoning",
		"summary": []any{map[string]any{
			"type": "summary_text", "text": text,
		}},
	}
	events := []string{
		sseEvent("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": c.reasoning.itemID,
			"output_index": c.reasoning.outputIndex, "summary_index": 0, "text": text,
		}),
		sseEvent("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": c.reasoning.itemID,
			"output_index": c.reasoning.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		}),
		outputItemDoneEvent(c.reasoning.outputIndex, item),
	}
	c.outputItems = append(c.outputItems, indexedOutputItem{c.reasoning.outputIndex, item})
	c.reasoning.done = true
	return events
}

func (c *SSEConverter) finalizeText() []string {
	if !c.text.added || c.text.done {
		return nil
	}
	text := c.text.text.String()
	item := map[string]any{
		"id": c.text.itemID, "type": "message", "status": "completed",
		"role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	}
	events := []string{
		sseEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": c.text.itemID,
			"output_index": c.text.outputIndex, "content_index": 0, "text": text,
		}),
		sseEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": c.text.itemID,
			"output_index": c.text.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}),
		outputItemDoneEvent(c.text.outputIndex, item),
	}
	c.outputItems = append(c.outputItems, indexedOutputItem{c.text.outputIndex, item})
	c.text.done = true
	return events
}

func (c *SSEConverter) finalizeTools() []string {
	var events []string
	for _, key := range c.sortedToolKeys() {
		state := c.tools[key]
		if state.done {
			continue
		}
		// Whitespace-only names correspond to no published tool and must be
		// dropped exactly like empty ones, otherwise they would pose as
		// "this turn still has tool calls" and bypass the dropped-call
		// failure criterion below.
		if strings.TrimSpace(state.name) == "" {
			state.done = true
			c.droppedCalls++
			continue
		}
		if !state.added {
			assigned := c.nextOutputIndex
			c.nextOutputIndex++
			state.added = true
			if state.callID == "" {
				state.callID = fmt.Sprintf("call_%d", key)
			}
			state.outputIndex = assigned
			state.itemID = responseToolCallItemIDFromChatName(state.callID, state.name, c.toolContext)
			item := responseToolCallItemFromChatName(state.itemID, "in_progress", state.callID, state.name, "", state.reasoningContent, c.toolContext)
			events = append(events, outputItemAddedEvent(assigned, item))
		}
		arguments := canonicalizeToolArgumentsStr(state.arguments.String())
		isCustomTool := c.toolContext.IsCustomToolChatName(state.name)
		item := responseToolCallItemFromChatName(state.itemID, "completed", state.callID, state.name, arguments, state.reasoningContent, c.toolContext)
		state.done = true
		c.outputItems = append(c.outputItems, indexedOutputItem{state.outputIndex, item})
		if isCustomTool {
			input := customToolInputFromChatArguments(arguments)
			if input != "" {
				events = append(events, sseEvent("response.custom_tool_call_input.delta", map[string]any{
					"type": "response.custom_tool_call_input.delta", "item_id": state.itemID,
					"output_index": state.outputIndex, "delta": input,
				}))
			}
			events = append(events, sseEvent("response.custom_tool_call_input.done", map[string]any{
				"type": "response.custom_tool_call_input.done", "item_id": state.itemID,
				"output_index": state.outputIndex, "input": input,
			}))
		} else {
			events = append(events, sseEvent("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": state.itemID,
				"output_index": state.outputIndex, "arguments": arguments,
			}))
		}
		events = append(events, outputItemDoneEvent(state.outputIndex, item))
	}
	return events
}

func (c *SSEConverter) completedOutputItems() []any {
	entries := append([]indexedOutputItem(nil), c.outputItems...)
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].index < entries[j].index
	})
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.item)
	}
	return out
}

func (c *SSEConverter) baseResponse(status string, output []any) map[string]any {
	usage := c.latestUsage
	if !c.hasUsage {
		usage = chatUsageToResponsesUsage(nil)
	}
	return map[string]any{
		"id":         c.responseID,
		"object":     "response",
		"created_at": c.createdAt,
		"status":     status,
		"model":      c.model,
		"output":     output,
		"usage":      usage,
	}
}

func (c *SSEConverter) failedEvent(message, errorType string) string {
	c.completed = true
	c.failedMessage = message
	c.failedType = errorType
	errorObject := map[string]any{"message": message}
	if errorType != "" {
		errorObject["type"] = errorType
	}
	response := c.baseResponse("failed", c.completedOutputItems())
	response["error"] = errorObject
	return sseEvent("response.failed", map[string]any{
		"type": "response.failed", "response": response,
	})
}

// noteRetained accounts streamed text against the bounded conversion budget
// shared by all long-lived state. Convert surfaces
// ErrSSEStateTooLarge once the budget is exceeded.
func (c *SSEConverter) noteRetained(size int) {
	c.retainedBytes += size
}

// CanonicalResponse returns the response chain accumulated by the converter.
// It is intentionally independent from the emitted SSE bytes: the returned
// value is safe to marshal and persist for GET/previous_response_id.
func (c *SSEConverter) CanonicalResponse() map[string]any {
	if c == nil || !c.responseStarted {
		return nil
	}
	status := "in_progress"
	if c.failedMessage != "" {
		status = "failed"
	} else if c.completed {
		status = responseStatusFromFinishReason(c.finishReason)
	}
	usage := c.latestUsage
	if !c.hasUsage {
		usage = chatUsageToResponsesUsage(nil)
	}
	response := map[string]any{
		"id":     c.responseID,
		"object": "response",
		"model":  c.model,
		"status": status,
		"output": c.completedOutputItems(),
		"usage":  usage,
	}
	if c.failedMessage != "" {
		errorObject := map[string]any{"message": c.failedMessage}
		if c.failedType != "" {
			errorObject["type"] = c.failedType
		}
		response["error"] = errorObject
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": responseIncompleteReason(c.finishReason)}
	}
	return response
}

// ConvertSSE converts a complete Chat SSE byte stream into the Responses SSE
// lifecycle. For streaming proxy use SSEConverter, which carries state
// between writes.
func ConvertSSE(input []byte) ([]byte, error) {
	c := NewSSEConverter()
	first, err := c.Convert(input)
	if err != nil {
		return nil, err
	}
	last, err := c.Flush()
	if err != nil {
		return nil, err
	}
	return append(first, last...), nil
}

// ResponseIDFromSSE exposes the deterministic id used by SSEConverter. It is
// useful to callers that need to correlate a converted stream with affinity
// state without parsing every event themselves.
func ResponseIDFromSSE(input []byte) string {
	converter := NewSSEConverter()
	_, _ = converter.Convert(input)
	if converter.responseID != "" && converter.responseID != "resp_"+shortSHA256Hex(nil) {
		return converter.responseID
	}
	_, _ = converter.Flush()
	if converter.responseID != "" {
		return converter.responseID
	}
	for _, line := range strings.Split(string(input), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if id, _ := chunk["id"].(string); id != "" {
			return responseIDFromChatID(id)
		}
		return "resp_" + shortSHA256Hex([]byte(payload))
	}
	return ""
}
