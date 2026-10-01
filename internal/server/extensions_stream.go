package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

type extensionStreamWriter struct {
	client                http.ResponseWriter
	request               *http.Request
	state                 *extensionRequestState
	header                http.Header
	status                int
	committed             bool
	pending               []byte
	raw                   bytes.Buffer
	endpoint              string
	publicID              string
	model                 string
	created               any
	text                  strings.Builder
	callsByIndex          map[int]*extensionCall
	callOrder             []int
	finish                map[string]any
	completed             map[string]any
	usage                 map[string]any
	responsesCreated      bool
	maxOutputIndex        int
	anthropicIndexes      map[int]int
	anthropicNextIndex    int
	anthropicMessageDelta map[string]any
}

func newExtensionStreamWriter(w http.ResponseWriter, r *http.Request, state *extensionRequestState) *extensionStreamWriter {
	return &extensionStreamWriter{client: w, request: r, state: state, header: make(http.Header), endpoint: state.meta.Endpoint, callsByIndex: map[int]*extensionCall{}, publicID: state.streamID, model: state.streamModel, created: state.streamCreated, responsesCreated: state.streamCreatedFlag, maxOutputIndex: -1, anthropicIndexes: map[int]int{}, anthropicNextIndex: state.streamOutputOffset}
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
			w.usage = maxUsage(w.usage, usage)
		}
		return nil
	case "message_stop":
		return nil
	case "content_block_start":
		index := intNumber(payload["index"])
		block, _ := payload["content_block"].(map[string]any)
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
		if index > w.maxOutputIndex {
			w.maxOutputIndex = index
		}
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
	choice, _ := choices[0].(map[string]any)
	if finish := choice["finish_reason"]; finish != nil {
		w.finish = payload
		return nil
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return nil
	}
	if content, ok := delta["content"].(string); ok {
		w.text.WriteString(content)
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			fragment, _ := raw.(map[string]any)
			index := intNumber(fragment["index"])
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
	if len(delta) == 0 {
		return nil
	}
	return w.emit("", payload)
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
	if strings.Contains(name, "function_call") || strings.Contains(name, "custom_tool_call") || strings.Contains(name, "tool_search_call") {
		if name == "response.function_call_arguments.done" {
			index := intNumber(payload["output_index"])
			call := w.callsByIndex[index]
			if call == nil {
				call = &extensionCall{}
				w.callsByIndex[index] = call
				w.callOrder = append(w.callOrder, index)
			}
			call.ID, _ = payload["call_id"].(string)
			call.Name, _ = payload["name"].(string)
			call.Arguments, _ = payload["arguments"].(string)
		}
		return nil
	}
	if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
		index := intNumber(payload["output_index"])
		if name == "response.output_item.done" {
			call := w.callsByIndex[index]
			if call == nil {
				call = &extensionCall{}
				w.callsByIndex[index] = call
				w.callOrder = append(w.callOrder, index)
			}
			call.ID, _ = item["call_id"].(string)
			call.Name, _ = item["name"].(string)
			call.Arguments, _ = item["arguments"].(string)
		}
		return nil
	}
	if name == "response.output_text.delta" {
		if delta, ok := payload["delta"].(string); ok {
			w.text.WriteString(delta)
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
		if int(index) > w.maxOutputIndex {
			w.maxOutputIndex = int(index)
		}
		payload["output_index"] = int(index) + w.state.streamOutputOffset
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
		payload, err = extensionMapHook(w.request.Context(), item, "onStreamEvent", w.state.meta, payload)
		if err != nil && !item.Manifest.ContinueOnError {
			return err
		}
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
	if w.endpoint == "chat.completions" {
		out := make([]any, 0, len(calls))
		for _, call := range calls {
			out = append(out, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": w.text.String(), "tool_calls": out}}}}
	}
	out := make([]any, 0, len(calls))
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
		}
		return w.emit("response.completed", w.completed)
	}
	return nil
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
			index := w.anthropicNextIndex
			w.anthropicNextIndex++
			var input any
			if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
				return fmt.Errorf("invalid client tool input: %w", err)
			}
			block := map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input}
			if err := w.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block}); err != nil {
				return err
			}
			if err := w.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
		}
		w.anthropicMessageDelta = map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}}
		return w.Finish()
	}
	for i, call := range calls {
		item := map[string]any{"type": "function_call", "id": call.ID, "call_id": call.ID, "name": call.Name, "arguments": call.Arguments, "status": "completed"}
		index := w.state.streamOutputOffset + w.maxOutputIndex + 1 + i
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
	for round := 0; round < cfg.Extensions.EffectiveMaxToolRounds(); round++ {
		if err := r.Context().Err(); err != nil {
			return false
		}
		prepared, err := json.Marshal(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		roundRequest := r.Clone(context.WithValue(r.Context(), extensionContextKey{}, state))
		setExtensionBody(roundRequest, prepared)
		stream := newExtensionStreamWriter(w, roundRequest, state)
		next.ServeHTTP(stream, roundRequest)
		if stream.status >= 400 {
			notifyExtensionsError(r.Context(), state, fmt.Sprintf("upstream returned HTTP %d", stream.status))
			_ = stream.Finish()
			return false
		}
		state.streamText += stream.text.String()
		state.streamUsage = sumUsage(state.streamUsage, stream.usage)
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
			return true
		}
		if len(clientCalls) > 0 {
			publicCalls, err := s.saveExtensionMixed(r.Context(), state, request, stream.ContinueResponse(), serverCalls, clientCalls, state.meta.Endpoint)
			if err != nil {
				stream.Fail(err)
				return false
			}
			if err := stream.FinishClientCalls(publicCalls); err != nil {
				stream.Fail(err)
				return false
			}
			return true
		}
		if round+1 >= cfg.Extensions.EffectiveMaxToolRounds() {
			stream.Fail(fmt.Errorf("extension tool round limit reached"))
			return false
		}
		if stream.maxOutputIndex >= 0 {
			state.streamOutputOffset += stream.maxOutputIndex + 1
		}
		if err := extensionContinue(r.Context(), state, request, stream.ContinueResponse(), serverCalls, state.meta.Endpoint); err != nil {
			stream.Fail(err)
			return false
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
