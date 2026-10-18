package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"log/slog"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

type extensionStreamWriter struct {
	client           http.ResponseWriter
	request          *http.Request
	state            *extensionRequestState
	header           http.Header
	status           int
	committed        bool
	pending          []byte
	raw              bytes.Buffer
	endpoint         string
	publicID         string
	model            string
	created          any
	text             strings.Builder
	callsByIndex     map[int]*extensionCall
	callOrder        []int
	finish           map[string]any
	completed        map[string]any
	usage            map[string]any
	responsesCreated bool
	// outputNextIndex is the absolute output_index the next Responses item takes:
	// the upstream's own items advance it, so an artifact emitted after them can
	// never repeat or skip an index.
	outputNextIndex       int
	anthropicIndexes      map[int]int
	anthropicNextIndex    int
	anthropicMessageDelta map[string]any
	// reasoning accumulates the round's Chat reasoning deltas
	// (reasoning_content, or OpenRouter's reasoning). A thinking-mode upstream
	// such as DeepSeek refuses the continuation without it ("The
	// reasoning_content in the thinking mode must be passed back"), so the
	// assistant turn this bridge rebuilds has to carry it.
	reasoning     strings.Builder
	reasoningItem map[string]any
	// thoughtBlocks and thoughtParts keep the thinking of the other dialects
	// verbatim, because their signatures cannot be rebuilt from text: Anthropic
	// extended thinking (thinking/redacted_thinking, signature included) and
	// Gemini thought parts (thoughtSignature). Both must travel back with the
	// tool result of the round they belong to.
	thoughtBlocks map[int]*extensionThoughtBlock
	thoughtOrder  []int
	thoughtParts  []any
}

// extensionThoughtBlock is one Anthropic thinking block of a streaming round:
// the block the upstream opened, the thinking text it streamed, and the
// signature that arrived with it.
type extensionThoughtBlock struct {
	start     map[string]any
	text      strings.Builder
	signature string
}

func (b *extensionThoughtBlock) render() map[string]any {
	if b == nil {
		return nil
	}
	if b.start["type"] == "redacted_thinking" {
		return b.start
	}
	// An unsigned thinking block is not a valid assistant turn for Anthropic:
	// echoing one turns a working continuation into a rejected request, so a
	// block the upstream never signed is left out (its reasoning is lost either
	// way, and this keeps the request legal).
	if b.signature == "" {
		return nil
	}
	return map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": b.signature}
}

func newExtensionStreamWriter(w http.ResponseWriter, r *http.Request, state *extensionRequestState) *extensionStreamWriter {
	return &extensionStreamWriter{client: w, request: r, state: state, header: make(http.Header), endpoint: state.meta.Endpoint, callsByIndex: map[int]*extensionCall{}, publicID: state.streamID, model: state.streamModel, created: state.streamCreated, responsesCreated: state.streamCreatedFlag, anthropicIndexes: map[int]int{}, anthropicNextIndex: state.streamOutputOffset, outputNextIndex: state.streamOutputOffset, thoughtBlocks: map[int]*extensionThoughtBlock{}}
}

func (w *extensionStreamWriter) Header() http.Header { return w.header }
func (w *extensionStreamWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *extensionStreamWriter) Flush() {
	if flusher, ok := w.client.(http.Flusher); ok && w.committed {
		flusher.Flush()
	}
}

func (w *extensionStreamWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 {
		if w.raw.Len()+len(p) <= backendTransformBodyLimit {
			_, _ = w.raw.Write(p)
		}
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	if len(w.pending) > 4<<20 {
		return 0, fmt.Errorf("extension SSE event exceeds 4 MiB")
	}
	for {
		idx := bytes.Index(w.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), w.pending[:idx+2]...)
		w.pending = w.pending[idx+2:]
		if err := w.consume(frame); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *extensionStreamWriter) consume(frame []byte) error {
	var eventName, data string
	for _, line := range strings.Split(strings.TrimSpace(string(frame)), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if data == "" || data == "[DONE]" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return fmt.Errorf("invalid extension SSE JSON: %w", err)
	}
	if w.endpoint == "chat.completions" {
		return w.consumeChat(payload)
	}
	if w.endpoint == "anthropic.messages" {
		return w.consumeAnthropic(eventName, payload)
	}
	if w.endpoint == "gemini.generateContent" {
		return w.consumeGemini(payload)
	}
	return w.consumeResponses(eventName, payload)
}

func (w *extensionStreamWriter) consumeAnthropic(eventName string, payload map[string]any) error {
	name, _ := payload["type"].(string)
	if name == "" {
		name = eventName
	}
	switch name {
	case "message_start":
		message, _ := payload["message"].(map[string]any)
		if usage, ok := message["usage"].(map[string]any); ok {
			w.usage = maxUsage(w.usage, usage)
		}
		if w.state.streamCreatedFlag {
			return nil
		}
		w.state.streamCreatedFlag = true
		if id, _ := message["id"].(string); id != "" {
			w.publicID = id
			w.state.streamID = id
		}
		if model, _ := message["model"].(string); model != "" {
			w.model = model
			w.state.streamModel = model
		}
		return w.emit(name, payload)
	case "message_delta":
		w.anthropicMessageDelta = payload
		if usage, ok := payload["usage"].(map[string]any); ok {
			// Anthropic documents message_delta usage as cumulative and it may
			// repeat the input counts. A converted stream seeds message_start
			// with an estimate, so the delta must replace those fields rather
			// than lose to max() against the estimate.
			w.usage = latestUsage(w.usage, usage)
		}
		return nil
	case "message_stop":
		return nil
	case "content_block_start":
		index := intNumber(payload["index"])
		block, _ := payload["content_block"].(map[string]any)
		if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
			// Held verbatim: the continuation must carry this block (and its
			// signature) back with the tool result, and a rebuilt block would
			// arrive unsigned.
			w.thoughtBlocks[index] = &extensionThoughtBlock{start: block}
			w.thoughtOrder = append(w.thoughtOrder, index)
		}
		if block["type"] == "tool_use" {
			call := w.callsByIndex[index]
			if call == nil {
				call = &extensionCall{}
				w.callsByIndex[index] = call
				w.callOrder = append(w.callOrder, index)
			}
			call.ID, _ = block["id"].(string)
			call.Name, _ = block["name"].(string)
			if input, ok := block["input"].(map[string]any); ok {
				encoded, _ := json.Marshal(input)
				call.Arguments = string(encoded)
			}
			return nil
		}
		w.anthropicIndexes[index] = w.anthropicNextIndex
		w.anthropicNextIndex++
		payload["index"] = w.anthropicIndexes[index]
		return w.emit(name, payload)
	case "content_block_delta":
		index := intNumber(payload["index"])
		if call := w.callsByIndex[index]; call != nil {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				fragment, _ := delta["partial_json"].(string)
				if fragment != "" {
					if call.Arguments == "{}" {
						call.Arguments = ""
					}
					call.Arguments += fragment
				}
			}
			return nil
		}
		if index, ok := w.anthropicIndexes[index]; ok {
			payload["index"] = index
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta["type"] == "text_delta" {
			if text, ok := delta["text"].(string); ok {
				w.text.WriteString(text)
			}
		}
		if block := w.thoughtBlocks[intNumber(payload["index"])]; block != nil {
			switch delta["type"] {
			case "thinking_delta":
				if text, ok := delta["thinking"].(string); ok {
					block.text.WriteString(text)
				}
			case "signature_delta":
				if signature, ok := delta["signature"].(string); ok {
					block.signature = signature
				}
			}
		}
		return w.emit(name, payload)
	case "content_block_stop":
		index := intNumber(payload["index"])
		if call := w.callsByIndex[index]; call != nil {
			if call.Arguments == "" {
				call.Arguments = "{}"
			}
			return nil
		}
		if index, ok := w.anthropicIndexes[index]; ok {
			payload["index"] = index
		}
		return w.emit(name, payload)
	default:
		return w.emit(name, payload)
	}
}

func (w *extensionStreamWriter) consumeChat(payload map[string]any) error {
	if id, _ := payload["id"].(string); w.publicID == "" && id != "" {
		w.publicID = id
	}
	if w.publicID != "" {
		payload["id"] = w.publicID
	}
	if model, _ := payload["model"].(string); w.model == "" {
		w.model = model
	}
	if w.created == nil {
		w.created = payload["created"]
	}
	w.state.streamID, w.state.streamModel, w.state.streamCreated = w.publicID, w.model, w.created
	// Backends like vLLM (enable_force_include_usage) attach the cumulative
	// usage to every frame, including content frames. Track the high-water
	// mark and keep processing the choices below — dropping the frame here
	// erased the whole reply for those backends.
	if usage, ok := payload["usage"].(map[string]any); ok {
		w.usage = maxUsage(w.usage, usage)
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	bufferedFinish := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if finish := choice["finish_reason"]; finish != nil {
			// An alternate choice never keeps its calls (see the tool-call branch
			// below), so it must not end on a tool-call finish either: the buffered
			// path downgrades those choices the same way.
			if intNumber(choice["index"]) != 0 {
				if reason, _ := finish.(string); reason == "tool_calls" {
					choice["finish_reason"] = "stop"
				}
			}
			w.finish = payload
			bufferedFinish = true
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if content, ok := delta["content"].(string); ok {
			w.text.WriteString(content)
		}
		if reasoning := firstStringValue(delta, "reasoning_content", "reasoning"); reasoning != "" {
			w.reasoning.WriteString(reasoning)
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			// Only the first choice joins the extension loop, exactly as the
			// buffered path does: the continuation carries one assistant turn, so a
			// call in an alternate choice could not be executed with its result
			// spliced in. Its fragment is dropped from the delta below (so it never
			// reaches the client unexecuted), and the extension's own calls are
			// withheld from the round like any other.
			choiceIndex := intNumber(choice["index"])
			if choiceIndex != 0 {
				delete(delta, "tool_calls")
				continue
			}
			for _, raw := range calls {
				fragment, _ := raw.(map[string]any)
				index := choiceIndex*1000 + intNumber(fragment["index"])
				call := w.callsByIndex[index]
				if call == nil {
					call = &extensionCall{}
					w.callsByIndex[index] = call
					w.callOrder = append(w.callOrder, index)
				}
				if id, _ := fragment["id"].(string); id != "" {
					call.ID = id
				}
				function, _ := fragment["function"].(map[string]any)
				if name, _ := function["name"].(string); name != "" {
					call.Name += name
				}
				if args, _ := function["arguments"].(string); args != "" {
					call.Arguments += args
				}
			}
			delete(delta, "tool_calls")
		}
	}
	// Drop the payload when nothing visible remains in any choice.
	visible := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if delta, ok := choice["delta"].(map[string]any); ok && len(delta) > 0 {
			visible = true
		}
		if choice["finish_reason"] != nil {
			visible = true
		}
	}
	if !visible {
		return nil
	}
	// A finish frame is held back until the round is classified: emitting it here
	// ended the client's stream before the tool call it belongs to had been handed
	// over (and Finish() emits it once the round is settled). This payload may mix
	// a delta with the finish frame, so the buffered flag is what decides.
	if bufferedFinish {
		return nil
	}
	return w.emit("", payload)
}

// consumeGemini accumulates one generateContent chunk. The model's tool calls
// are held back so a server-side tool never reaches the client unexecuted; text
// and other parts stream through as they arrive.
func (w *extensionStreamWriter) consumeGemini(payload map[string]any) error {
	if usage, ok := payload["usageMetadata"].(map[string]any); ok {
		w.usage = maxUsage(w.usage, usage)
	}
	candidates, _ := payload["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	visible := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		call := part["functionCall"]
		if call == nil {
			call = part["function_call"]
		}
		if entry, ok := call.(map[string]any); ok {
			name, _ := entry["name"].(string)
			arguments, err := json.Marshal(entry["args"])
			if err != nil {
				arguments = []byte("{}")
			}
			// Gemini streams one functionCall per part, each chunk starting at
			// part index 0, so the in-chunk index cannot identify a call: keying
			// by it collapsed a round with two calls into one and lost the other.
			// The part itself is kept: it may carry a thought signature the
			// continuation has to echo back.
			key := len(w.callOrder)
			w.callsByIndex[key] = &extensionCall{ID: geminiCallID(key), Name: name, Arguments: string(arguments), RawPart: part}
			w.callOrder = append(w.callOrder, key)
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			if thought, _ := part["thought"].(bool); !thought {
				w.text.WriteString(text)
			}
		}
		if thought, _ := part["thought"].(bool); thought || part["thoughtSignature"] != nil {
			// A thought part carries the signature the continuation has to echo.
			w.thoughtParts = append(w.thoughtParts, part)
		}
		visible = append(visible, part)
	}
	if len(visible) == 0 && candidate["finishReason"] == nil {
		return nil
	}
	out := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": visible}, "index": 0}}}
	if reason := candidate["finishReason"]; reason != nil {
		out["candidates"].([]any)[0].(map[string]any)["finishReason"] = reason
		// The finish reason travels only once the round is classified; a
		// tool-call finish would end the client's stream prematurely.
		if len(w.callOrder) > 0 && reason == "STOP" {
			delete(out["candidates"].([]any)[0].(map[string]any), "finishReason")
		} else {
			w.finish = out
			return nil
		}
	}
	return w.emit("", out)
}

func (w *extensionStreamWriter) consumeResponses(eventName string, payload map[string]any) error {
	name, _ := payload["type"].(string)
	if name == "" {
		name = eventName
	}
	if name == "response.created" {
		if w.responsesCreated {
			return nil
		}
		w.responsesCreated = true
		w.state.streamCreatedFlag = true
		if response, ok := payload["response"].(map[string]any); ok {
			if id, _ := response["id"].(string); id != "" {
				w.publicID = id
			}
		}
		w.state.streamID = w.publicID
	}
	if name == "response.completed" || name == "response.incomplete" || name == "response.cancelled" {
		w.completed = payload
		if response, ok := payload["response"].(map[string]any); ok {
			if usage, ok := response["usage"].(map[string]any); ok {
				w.usage = sumUsage(w.usage, usage)
			}
		}
		return nil
	}
	if strings.Contains(name, "function_call_arguments") {
		// Native Responses streams report the call's identity on
		// output_item.added and stream the arguments separately.
		index := intNumber(payload["output_index"])
		call := w.callsByIndex[index]
		if call == nil {
			call = &extensionCall{}
			w.callsByIndex[index] = call
			w.callOrder = append(w.callOrder, index)
		}
		switch name {
		case "response.function_call_arguments.delta":
			if delta, ok := payload["delta"].(string); ok {
				call.Arguments += delta
			}
		case "response.function_call_arguments.done":
			if arguments, ok := payload["arguments"].(string); ok {
				call.Arguments = arguments
			}
			if id, ok := payload["call_id"].(string); ok && id != "" {
				call.ID = id
			}
			if functionName, ok := payload["name"].(string); ok && functionName != "" {
				call.Name = functionName
			}
		}
		return nil
	}
	if strings.Contains(name, "custom_tool_call") || strings.Contains(name, "tool_search_call") {
		return nil
	}
	if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
		index := intNumber(payload["output_index"])
		call := w.callsByIndex[index]
		if call == nil {
			call = &extensionCall{}
			w.callsByIndex[index] = call
			w.callOrder = append(w.callOrder, index)
		}
		if name == "response.output_item.added" || name == "response.output_item.done" {
			if id, ok := item["call_id"].(string); ok && id != "" {
				call.ID = id
			}
			if functionName, ok := item["name"].(string); ok && functionName != "" {
				call.Name = functionName
			}
			if arguments, ok := item["arguments"].(string); ok && arguments != "" {
				call.Arguments = arguments
			}
		}
		return nil
	}
	if name == "response.output_text.delta" {
		if delta, ok := payload["delta"].(string); ok {
			w.text.WriteString(delta)
		}
	}
	if name == "response.reasoning_summary_text.delta" || name == "response.reasoning_text.delta" {
		if delta, ok := payload["delta"].(string); ok {
			w.reasoning.WriteString(delta)
		}
	}
	if item, ok := payload["item"].(map[string]any); ok && item["type"] == "reasoning" {
		// The item itself is kept: it may carry a summary, plain reasoning text
		// or an encrypted blob, and the continuation should hand back exactly
		// what the upstream produced rather than a rebuilt approximation.
		if name == "response.output_item.done" || w.reasoningItem == nil {
			w.reasoningItem = item
		}
	}
	if w.publicID != "" {
		if _, ok := payload["response_id"]; ok {
			payload["response_id"] = w.publicID
		}
		if response, ok := payload["response"].(map[string]any); ok {
			response["id"] = w.publicID
		}
	}
	if index, ok := payload["output_index"].(float64); ok {
		absolute := int(index) + w.state.streamOutputOffset
		if absolute >= w.outputNextIndex {
			w.outputNextIndex = absolute + 1
		}
		payload["output_index"] = absolute
	}
	return w.emit(name, payload)
}

func (w *extensionStreamWriter) emit(event string, payload map[string]any) error {
	for i := len(w.state.items) - 1; i >= 0; i-- {
		item := w.state.items[i]
		if !hasExtensionHook(item, "onStreamEvent") {
			continue
		}
		var err error
		transformed, err := extensionMapHook(w.request.Context(), item, "onStreamEvent", w.state.meta, payload)
		if err != nil && !item.Manifest.ContinueOnError {
			// A capacity limit is not a broken transform: the frame is passed
			// through unchanged (note that the helper returns the input on
			// error) so the stream survives, and the reason is logged.
			if errors.Is(err, extensions.ErrHookInputTooLarge) {
				slog.Warn("extension stream hook skipped: payload too large",
					"extension", item.Manifest.ID, "request_id", w.state.meta.RequestID, "error", err)
				continue
			}
			return err
		}
		payload = transformed
	}
	if !w.committed {
		copyHeaders(w.client.Header(), w.header)
		w.client.Header().Set("Content-Type", "text/event-stream")
		w.client.Header().Set("Cache-Control", "no-cache")
		w.client.WriteHeader(w.status)
		w.committed = true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w.client, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w.client, "data: %s\n\n", data); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// DropWithheld removes calls for tools the round did not offer. Nothing about
// them has reached the client yet: the bridge holds tool-call fragments until
// the round is classified. Dropping them here is what lets the turn end with
// the model's text instead of a call the client cannot execute.
func (w *extensionStreamWriter) DropWithheld(names map[string]bool) bool {
	if len(names) == 0 || len(w.callOrder) == 0 {
		return false
	}
	dropped := false
	kept := make([]int, 0, len(w.callOrder))
	for _, index := range w.callOrder {
		call := w.callsByIndex[index]
		if call != nil && names[call.Name] {
			dropped = true
			continue
		}
		kept = append(kept, index)
	}
	if !dropped {
		return false
	}
	w.callOrder = kept
	if w.finish != nil {
		if choices, ok := w.finish["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if finish, _ := choice["finish_reason"].(string); finish == "tool_calls" {
					choice["finish_reason"] = "stop"
				}
			}
		}
	}
	if w.anthropicMessageDelta != nil {
		if delta, ok := w.anthropicMessageDelta["delta"].(map[string]any); ok {
			if stop, _ := delta["stop_reason"].(string); stop == "tool_use" {
				delta["stop_reason"] = "end_turn"
			}
		}
	}
	if w.completed != nil {
		if response, ok := w.completed["response"].(map[string]any); ok {
			if output, ok := response["output"].([]any); ok {
				visible := make([]any, 0, len(output))
				for _, raw := range output {
					item, _ := raw.(map[string]any)
					if item["type"] == "function_call" {
						if name, _ := item["name"].(string); names[name] {
							continue
						}
					}
					visible = append(visible, raw)
				}
				response["output"] = visible
			}
		}
	}
	return true
}

func (w *extensionStreamWriter) Calls() []extensionCall {
	sort.Ints(w.callOrder)
	result := make([]extensionCall, 0, len(w.callOrder))
	for _, index := range w.callOrder {
		call := *w.callsByIndex[index]
		if call.ID != "" && call.Name != "" {
			result = append(result, call)
		}
	}
	return result
}

func (w *extensionStreamWriter) ContinueResponse() map[string]any {
	calls := w.Calls()
	if w.endpoint == "anthropic.messages" {
		content := make([]any, 0, len(calls)+1)
		for _, index := range w.thoughtOrder {
			if block := w.thoughtBlocks[index].render(); block != nil {
				content = append(content, block)
			}
		}
		if text := w.text.String(); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, call := range calls {
			var input any
			if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
		}
		return map[string]any{"content": content, "stop_reason": "tool_use"}
	}
	if w.endpoint == "gemini.generateContent" {
		// The round's model turn as Gemini writes it: the text and the calls the
		// round produced. extensionContinue echoes this back verbatim (thought
		// signatures included) before the functionResponse parts; without it the
		// results had no call to attach to and the converter dropped them as
		// orphans.
		parts := make([]any, 0, len(calls)+len(w.thoughtParts)+1)
		parts = append(parts, w.thoughtParts...)
		if text := w.text.String(); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, call := range calls {
			if call.RawPart != nil {
				// Echo the part as it arrived: a thought signature lives there, and
				// rebuilding the part from name and arguments alone would drop it.
				parts = append(parts, call.RawPart)
				continue
			}
			var args any = map[string]any{}
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": call.Name, "args": args}})
		}
		return map[string]any{"content": map[string]any{"role": "model", "parts": parts}}
	}
	if w.endpoint == "chat.completions" {
		out := make([]any, 0, len(calls))
		for _, call := range calls {
			out = append(out, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		message := map[string]any{"role": "assistant", "content": w.text.String(), "tool_calls": out}
		if reasoning := w.reasoning.String(); reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		return map[string]any{"choices": []any{map[string]any{"message": message}}}
	}
	// A Responses transcript carries text as message items. They belong in the
	// continuation too: dropping them left a text answer with no output.
	out := make([]any, 0, len(calls)+2)
	if item := w.continuationReasoningItem(); item != nil {
		// The reasoning item leads the assistant turn, exactly as the converter
		// writes it when a client sends one: the next round turns it back into
		// reasoning_content for a thinking-mode Chat upstream.
		out = append(out, item)
	}
	if text := w.text.String(); text != "" {
		out = append(out, map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for _, call := range calls {
		out = append(out, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments})
	}
	return map[string]any{"output": out}
}

func (w *extensionStreamWriter) Finish() error {
	if w.status >= 400 {
		if !w.committed {
			copyHeaders(w.client.Header(), w.header)
			w.client.WriteHeader(w.status)
			_, _ = w.client.Write(w.raw.Bytes())
		}
		return nil
	}
	if w.endpoint == "chat.completions" {
		if w.finish != nil {
			if err := w.emit("", w.finish); err != nil {
				return err
			}
		}
		if len(w.state.streamUsage) > 0 {
			usage := map[string]any{"id": w.publicID, "object": "chat.completion.chunk", "model": w.model, "created": w.created, "choices": []any{}, "usage": w.state.streamUsage}
			if err := w.emit("", usage); err != nil {
				return err
			}
		}
		if !w.committed {
			w.client.Header().Set("Content-Type", "text/event-stream")
			w.client.WriteHeader(http.StatusOK)
			w.committed = true
		}
		_, err := w.client.Write([]byte("data: [DONE]\n\n"))
		w.Flush()
		return err
	}
	if w.endpoint == "gemini.generateContent" {
		// A round that carried calls had its finishReason held back so the stream
		// would not end early; it still has to be delivered, or a Gemini client
		// never learns the turn completed.
		if w.finish != nil {
			if err := w.emit("", w.finish); err != nil {
				return err
			}
		}
		if !w.committed {
			copyHeaders(w.client.Header(), w.header)
			w.client.Header().Set("Content-Type", "text/event-stream")
			w.client.WriteHeader(http.StatusOK)
			w.committed = true
		}
		w.Flush()
		return nil
	}
	if w.endpoint == "anthropic.messages" {
		delta := w.anthropicMessageDelta
		if delta == nil {
			delta = map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}
		}
		delta["usage"] = w.state.streamUsage
		if err := w.emit("message_delta", delta); err != nil {
			return err
		}
		return w.emit("message_stop", map[string]any{"type": "message_stop"})
	}
	if w.completed != nil {
		if response, ok := w.completed["response"].(map[string]any); ok {
			response["usage"] = w.state.streamUsage
			response["id"] = w.publicID
			mergeExtensionStreamText(response, w.state.streamText)
			w.visibleCompletedItems(response)
		}
		return w.emit("response.completed", w.completed)
	}
	return nil
}

// emitAnthropicCallBlock writes one complete content block for a tool call.
//
// The arguments travel as an input_json_delta frame rather than inline: Claude
// Code's stream accumulator keeps a tool block's input only from deltas, so a
// block whose arguments arrived inline is replayed - and executed by the client -
// with an empty input (a client tool call then fails its own input validation
// with "provided as unknown"). Anthropic itself streams the delta form.
func (w *extensionStreamWriter) emitAnthropicCallBlock(blockType string, call extensionCall) error {
	index := w.anthropicNextIndex
	w.anthropicNextIndex++
	block := map[string]any{"type": blockType, "id": call.ID, "name": call.Name, "input": map[string]any{}}
	if err := w.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block}); err != nil {
		return err
	}
	if arguments := strings.TrimSpace(call.Arguments); arguments != "" && arguments != "{}" {
		delta := map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}}
		if err := w.emit("content_block_delta", delta); err != nil {
			return err
		}
	}
	return w.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

// EmitVisibleCalls writes the round's calls for a surface that reports
// server-side work: a server call becomes a server_tool_use block immediately
// followed by its result block, a caller-owned call stays a plain tool_use. The
// model's own call order is preserved.
func (w *extensionStreamWriter) EmitVisibleCalls(calls []extensionCall, serverIDs map[string]bool, outputs map[string]map[string]any) error {
	switch w.endpoint {
	case "responses":
		return w.emitResponsesVisibleCalls(calls, serverIDs, outputs)
	case "anthropic.messages":
	default:
		return nil
	}
	for _, call := range calls {
		if !serverIDs[call.ID] {
			if err := w.emitAnthropicCallBlock("tool_use", call); err != nil {
				return err
			}
			continue
		}
		artifact := claudeServerCall(call)
		index := w.anthropicNextIndex
		w.anthropicNextIndex++
		block := map[string]any{"type": artifact["type"], "id": artifact["id"], "name": artifact["name"], "input": map[string]any{}}
		if err := w.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block}); err != nil {
			return err
		}
		if encoded, err := json.Marshal(artifact["input"]); err == nil && len(encoded) > 2 {
			delta := map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}}
			if err := w.emit("content_block_delta", delta); err != nil {
				return err
			}
		}
		if err := w.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		if output, ok := outputs[call.ID]; ok {
			result := claudeServerResult(call, output)
			resultIndex := w.anthropicNextIndex
			w.anthropicNextIndex++
			if err := w.emit("content_block_start", map[string]any{"type": "content_block_start", "index": resultIndex, "content_block": result}); err != nil {
				return err
			}
			if err := w.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": resultIndex}); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitResponsesVisibleCalls writes the round's items: caller-owned calls stay
// function_call items, server calls become mcp_call items carrying their output.
// The artifact items are remembered on the request state so the final
// response.completed frame lists the whole turn, not only its last round.
func (w *extensionStreamWriter) emitResponsesVisibleCalls(calls []extensionCall, serverIDs map[string]bool, outputs map[string]map[string]any) error {
	for _, call := range calls {
		var item map[string]any
		if serverIDs[call.ID] {
			item = responsesCallItem(call, outputs[call.ID])
		} else {
			item = map[string]any{"type": "function_call", "id": call.ID, "call_id": call.ID, "name": call.Name, "arguments": call.Arguments, "status": "completed"}
		}
		index := w.outputNextIndex
		w.outputNextIndex++
		if err := w.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": w.publicID, "output_index": index, "item": item}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.publicID, "output_index": index, "item": item}); err != nil {
			return err
		}
		if serverIDs[call.ID] {
			w.state.visibleItems = append(w.state.visibleItems, visibleArtifact{SourceCallID: call.ID, Item: item})
		}
	}
	return nil
}

// visibleCompletedItems merges the artifact items of every round into the final
// completed response so a client that reads only that frame still sees the
// server work.
func (w *extensionStreamWriter) visibleCompletedItems(response map[string]any) {
	if len(w.state.visibleItems) == 0 {
		return
	}
	bySource := map[string]map[string]any{}
	for _, artifact := range w.state.visibleItems {
		bySource[artifact.SourceCallID] = artifact.Item
	}
	output, _ := response["output"].([]any)
	merged := make([]any, 0, len(w.state.visibleItems)+len(output))
	used := map[string]bool{}
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item != nil && item["type"] == "function_call" {
			id, _ := item["call_id"].(string)
			if id == "" {
				id, _ = item["id"].(string)
			}
			if artifact, ok := bySource[id]; ok {
				// The artifacts this turn reported replace the server calls they
				// stand for: keeping both listed the same call twice (once as the
				// artifact, once as a call with no result) in the frame clients
				// read and the daemon stores for previous_response_id.
				merged = append(merged, artifact)
				used[id] = true
				continue
			}
		}
		merged = append(merged, raw)
	}
	// Artifacts from earlier rounds are not part of this frame's own items; they
	// happened before it, so they lead.
	leading := make([]any, 0, len(w.state.visibleItems))
	for _, artifact := range w.state.visibleItems {
		if !used[artifact.SourceCallID] {
			leading = append(leading, artifact.Item)
		}
	}
	response["output"] = append(leading, merged...)
}

func (w *extensionStreamWriter) FinishClientCalls(calls []extensionCall) error {
	if len(calls) == 0 {
		return w.Finish()
	}
	if w.endpoint == "chat.completions" {
		fragments := make([]any, 0, len(calls))
		for i, call := range calls {
			fragments = append(fragments, map[string]any{"index": i, "id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		chunk := map[string]any{"id": w.publicID, "object": "chat.completion.chunk", "model": w.model, "created": w.created, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": fragments}, "finish_reason": nil}}}
		if err := w.emit("", chunk); err != nil {
			return err
		}
		if w.finish == nil {
			w.finish = map[string]any{"id": w.publicID, "object": "chat.completion.chunk", "model": w.model, "created": w.created, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}
		}
		return w.Finish()
	}
	if w.endpoint == "anthropic.messages" {
		for _, call := range calls {
			if err := w.emitAnthropicCallBlock("tool_use", call); err != nil {
				return err
			}
		}
		w.anthropicMessageDelta = map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}}
		return w.Finish()
	}
	if w.endpoint == "gemini.generateContent" {
		parts := make([]any, 0, len(calls))
		for _, call := range calls {
			var args any = map[string]any{}
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": call.Name, "args": args}})
		}
		if len(parts) > 0 {
			if err := w.emit("", map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0}}}); err != nil {
				return err
			}
		}
		if w.finish == nil {
			w.finish = map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": []any{}}, "finishReason": "STOP", "index": 0,
			}}}
		}
		return w.Finish()
	}
	for _, call := range calls {
		item := map[string]any{"type": "function_call", "id": call.ID, "call_id": call.ID, "name": call.Name, "arguments": call.Arguments, "status": "completed"}
		index := w.outputNextIndex
		w.outputNextIndex++
		if err := w.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": w.publicID, "output_index": index, "item": item}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.publicID, "output_index": index, "item": item}); err != nil {
			return err
		}
	}
	if w.completed != nil {
		if response, ok := w.completed["response"].(map[string]any); ok {
			output, _ := response["output"].([]any)
			visible := make([]any, 0, len(output))
			for _, raw := range output {
				item, _ := raw.(map[string]any)
				if item["type"] != "function_call" {
					visible = append(visible, raw)
					continue
				}
				for _, call := range calls {
					if item["name"] == call.Name {
						item["call_id"] = call.ID
						visible = append(visible, item)
						break
					}
				}
			}
			response["output"] = visible
		}
	}
	return w.Finish()
}

func (w *extensionStreamWriter) Fail(err error) {
	if !w.committed {
		http.Error(w.client, err.Error(), http.StatusBadGateway)
		return
	}
	if w.endpoint == "anthropic.messages" {
		_, _ = fmt.Fprintf(w.client, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", err.Error())
	} else {
		_, _ = fmt.Fprintf(w.client, "event: error\ndata: {\"error\":%q}\n\n", err.Error())
	}
	w.Flush()
}

func serveExtensionStream(s *Server, w http.ResponseWriter, r *http.Request, next http.Handler, cfg config.Config, state *extensionRequestState, request map[string]any) bool {
	rounds := cfg.Extensions.EffectiveMaxToolRounds()
	for round := 0; round < rounds; round++ {
		if err := r.Context().Err(); err != nil {
			return false
		}
		// The last round offers no server tools, so the model answers with what
		// it has instead of ending the turn on a call the extension can no
		// longer run. Everything it produces is streamed as usual.
		state.withholdTools = round+1 >= rounds
		prepared, err := json.Marshal(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		roundRequest := r.Clone(context.WithValue(r.Context(), extensionContextKey{}, state))
		setExtensionBody(roundRequest, prepared)
		// The stream bridge is a second forward path (the buffered rounds mark
		// their own requests); without the marker the protocol adapter strips a
		// hook-injected hosted tool and web search silently disappears on
		// streaming requests only.
		roundRequest = markInjectedHostedTools(roundRequest, request, state)
		stream := newExtensionStreamWriter(w, roundRequest, state)
		next.ServeHTTP(stream, roundRequest)
		if stream.status >= 400 {
			notifyExtensionsError(r.Context(), state, fmt.Sprintf("upstream returned HTTP %d", stream.status))
			_ = stream.Finish()
			return false
		}
		state.streamText += stream.text.String()
		state.streamUsage = sumUsage(state.streamUsage, stream.usage)
		stream.DropWithheld(state.withheldServerTools())
		calls := stream.Calls()
		serverCalls, clientCalls, err := classifyExtensionCalls(r.Context(), state, calls)
		if err != nil {
			stream.Fail(err)
			return false
		}
		if len(serverCalls) == 0 {
			if err := stream.FinishClientCalls(clientCalls); err != nil {
				stream.Fail(err)
				return false
			}
			s.persistVisibleStream(r.Context(), state, stream)
			return true
		}
		// A surface that reports server-side work hands the round over as it
		// happened: server calls travel back as artifacts, so the caller's replay
		// carries everything and no record has to survive the round trip.
		visibleTurn := serverToolArtifactsVisible(state.meta.Endpoint)
		serverIDs := make(map[string]bool, len(serverCalls))
		for _, call := range serverCalls {
			serverIDs[call.ID] = true
		}
		var demotedNames map[string]bool
		if len(clientCalls) > 0 && visibleTurn {
			if err := extensionContinue(r.Context(), state, request, stream.ContinueResponse(), serverCalls, state.meta.Endpoint); err != nil {
				stream.Fail(err)
				return false
			}
			if err := stream.EmitVisibleCalls(calls, serverIDs, state.serverOutputs); err != nil {
				stream.Fail(err)
				return false
			}
			stream.anthropicMessageDelta = map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}}
			if err := stream.Finish(); err != nil {
				stream.Fail(err)
				return false
			}
			s.persistVisibleStream(r.Context(), state, stream)
			return true
		}
		if len(clientCalls) > 0 {
			annotated := mergeClassifiedCalls(calls, append(append([]extensionCall{}, serverCalls...), clientCalls...))
			publicCalls, err := s.saveExtensionMixed(r.Context(), state, annotated, clientCalls, state.meta.Endpoint)
			if err != nil && !errors.Is(err, errExtensionContinuationTooLarge) {
				stream.Fail(err)
				return false
			}
			if err == nil {
				if err := stream.FinishClientCalls(publicCalls); err != nil {
					stream.Fail(err)
					return false
				}
				return true
			}
			// The conversation is too large to record how to resume a caller-run
			// tool. Nothing of the round's calls has reached the caller yet - the
			// bridge holds call fragments until the round is classified - so the
			// model is told they failed and the loop continues towards an answer
			// instead of an error frame. The calls stay in the continuation: the
			// failure results reference them.
			slog.Warn("extension continuation too large; caller tool calls are degraded",
				"model", state.meta.ResolvedModel, "endpoint", state.meta.Endpoint, "request_id", state.meta.RequestID, "error", err.Error())
			serverCalls = append(serverCalls, demoteUncontinuableClientCalls(state, clientCalls, err)...)
			// Neither the demoted calls nor the extension's own calls may reach a
			// caller that cannot run or continue them.
			demotedNames = extensionCallNames(serverCalls)
		}
		if round+1 >= rounds {
			// The rounds are spent. Everything the model produced has already
			// been streamed to the client, so the stream ends cleanly with that
			// answer rather than an error frame.
			slog.Warn("extension tool round limit reached; ending the stream with the model's last response",
				"rounds", rounds)
			if visibleTurn {
				if err := extensionContinue(r.Context(), state, request, stream.ContinueResponse(), serverCalls, state.meta.Endpoint); err != nil {
					stream.Fail(err)
					return false
				}
				if err := stream.EmitVisibleCalls(calls, serverIDs, state.serverOutputs); err != nil {
					stream.Fail(err)
					return false
				}
			}
			stream.DropWithheld(demotedNames)
			if err := stream.FinishClientCalls(nil); err != nil {
				stream.Fail(err)
				return false
			}
			s.persistVisibleStream(r.Context(), state, stream)
			return true
		}
		if err := extensionContinue(r.Context(), state, request, stream.ContinueResponse(), serverCalls, state.meta.Endpoint); err != nil {
			stream.Fail(err)
			return false
		}
		if visibleTurn {
			if err := stream.EmitVisibleCalls(calls, serverIDs, state.serverOutputs); err != nil {
				stream.Fail(err)
				return false
			}
		}
		// The next round's block/item indices must start past everything already on
		// the wire, and the artifacts were emitted last. Both surfaces keep one
		// absolute counter the writer advanced as it emitted; the round's own
		// upstream indices are relative to state.streamOutputOffset, never added to
		// it a second time.
		if state.meta.Endpoint == "anthropic.messages" {
			state.streamOutputOffset = stream.anthropicNextIndex
		} else {
			state.streamOutputOffset = stream.outputNextIndex
		}
	}
	return false
}

func mergeExtensionStreamText(response map[string]any, text string) {
	if text == "" {
		return
	}
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, partRaw := range content {
			part, _ := partRaw.(map[string]any)
			if part["type"] == "output_text" {
				part["text"] = text
				return
			}
		}
		item["content"] = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		return
	}
	response["output"] = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
}

func intNumber(value any) int { number, _ := value.(float64); return int(number) }

func sumUsage(total, delta map[string]any) map[string]any {
	if total == nil {
		total = map[string]any{}
	}
	for key, value := range delta {
		if n, ok := value.(float64); ok {
			previous, _ := total[key].(float64)
			total[key] = previous + n
		} else if nested, ok := value.(map[string]any); ok {
			previous, _ := total[key].(map[string]any)
			total[key] = sumUsage(previous, nested)
		} else if _, present := total[key]; !present {
			total[key] = value
		}
	}
	return total
}

// latestUsage merges cumulative usage where values in update replace those in
// total (unlike sumUsage, which adds successive deltas).
func latestUsage(total, update map[string]any) map[string]any {
	if total == nil {
		total = map[string]any{}
	}
	for key, value := range update {
		if nested, ok := value.(map[string]any); ok {
			previous, _ := total[key].(map[string]any)
			total[key] = latestUsage(previous, nested)
			continue
		}
		total[key] = value
	}
	return total
}

func maxUsage(total, current map[string]any) map[string]any {
	if total == nil {
		total = map[string]any{}
	}
	for key, value := range current {
		if n, ok := value.(float64); ok {
			previous, _ := total[key].(float64)
			if n > previous {
				total[key] = n
			}
		}
		if nested, ok := value.(map[string]any); ok {
			previous, _ := total[key].(map[string]any)
			total[key] = maxUsage(previous, nested)
		}
	}
	return total
}

var _ http.Flusher = (*extensionStreamWriter)(nil)

// firstStringValue returns the first non-empty string value among keys.
func firstStringValue(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// continuationReasoningItem is the reasoning item the continuation carries: the
// upstream's own item when the stream reported one, otherwise one built from the
// reasoning deltas it streamed. Nil when the round produced no reasoning.
func (w *extensionStreamWriter) continuationReasoningItem() map[string]any {
	if w.reasoningItem != nil {
		item := make(map[string]any, len(w.reasoningItem))
		for key, value := range w.reasoningItem {
			item[key] = value
		}
		delete(item, "status")
		return item
	}
	if text := w.reasoning.String(); text != "" {
		return map[string]any{
			"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": text}},
		}
	}
	return nil
}
