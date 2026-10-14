package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
)

func readResponsesRequestBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	limited := io.LimitReader(r.Body, backendTransformBodyLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("could not read Responses request: %w", err)
	}
	if len(body) > backendTransformBodyLimit {
		return nil, fmt.Errorf("responses request body exceeds %d bytes", backendTransformBodyLimit)
	}
	_ = r.Body.Close()
	return body, nil
}

// addPreviousResponseChainForModel appends the stored response output to a
// Chat adapter request. Responses affinity is model-scoped: accepting a
// previous id produced by another model could leak conversation content and
// would also give the adapter a chain it cannot faithfully continue.
func addPreviousResponseChainForModel(ctx context.Context, chatBody, responsesBody []byte, expectedModel string, st *store.Store) ([]byte, error) {
	if st == nil {
		return chatBody, nil
	}
	var request map[string]any
	if json.Unmarshal(responsesBody, &request) != nil {
		return chatBody, nil
	}
	previous, _ := request["previous_response_id"].(string)
	if strings.TrimSpace(previous) == "" {
		return chatBody, nil
	}
	affinity, found, err := st.GetResponseAffinity(ctx, previous, time.Now())
	if err != nil || !found {
		return chatBody, fmt.Errorf("previous_response_id %q is not available", previous)
	}
	if expectedModel = strings.TrimSpace(expectedModel); expectedModel != "" && strings.TrimSpace(affinity.Model) != "" && affinity.Model != expectedModel {
		return chatBody, fmt.Errorf("previous_response_id %q belongs to model %q, not %q", previous, affinity.Model, expectedModel)
	}
	var previousResponse map[string]any
	if err := json.Unmarshal(affinity.Response, &previousResponse); err != nil || previousResponse == nil {
		if err == nil {
			err = errors.New("stored response must be an object")
		}
		return chatBody, fmt.Errorf("previous_response_id %q has invalid stored response: %w", previous, err)
	}
	var output []any
	if rawOutput, exists := previousResponse["output"]; exists && rawOutput != nil {
		var ok bool
		output, ok = rawOutput.([]any)
		if !ok {
			return chatBody, fmt.Errorf("previous_response_id %q has malformed output array", previous)
		}
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		return chatBody, err
	}
	messages, _ := chat["messages"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			return chatBody, fmt.Errorf("previous_response_id %q contains a non-object output item", previous)
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "message":
			message, err := responseMessageToChat(item)
			if err != nil {
				return chatBody, fmt.Errorf("previous_response_id %q message: %w", previous, err)
			}
			messages = append(messages, message)
		case "reasoning":
			reasoning, err := responseReasoningToChat(item["summary"])
			if err != nil {
				return chatBody, fmt.Errorf("previous_response_id %q reasoning: %w", previous, err)
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": nil, "reasoning_content": reasoning})
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" {
				return chatBody, fmt.Errorf("previous_response_id %q contains malformed function_call", previous)
			}
			arguments, err := responseToolArgumentsToChat(item["arguments"])
			if err != nil {
				return chatBody, fmt.Errorf("previous_response_id %q function_call: %w", previous, err)
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments},
			}}})
		case "custom_tool_call", "tool_search_call":
			// The adapter response can also contain custom (freeform) and
			// tool-search calls; replay them with the same Chat wrapping the
			// request-side converter produces.
			callID, _ := item["call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				callID, _ = item["id"].(string)
			}
			name := "tool_search"
			var arguments any = "{}"
			if itemType == "custom_tool_call" {
				name, _ = item["name"].(string)
				input, hasInput := item["input"]
				if !hasInput {
					input = ""
				}
				arguments = map[string]any{"input": input}
			} else if raw, exists := item["arguments"]; exists && raw != nil {
				arguments = raw
			}
			if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" {
				return chatBody, fmt.Errorf("previous_response_id %q contains malformed %s", previous, itemType)
			}
			argumentsJSON, err := responseToolArgumentsToChat(arguments)
			if err != nil {
				return chatBody, fmt.Errorf("previous_response_id %q %s: %w", previous, itemType, err)
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": argumentsJSON},
			}}})
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			callID, _ := item["call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				return chatBody, fmt.Errorf("previous_response_id %q contains %s without call_id", previous, itemType)
			}
			output, err := responseToolOutputToChat(item["output"])
			if err != nil {
				return chatBody, fmt.Errorf("previous_response_id %q %s: %w", previous, itemType, err)
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
		default:
			return chatBody, fmt.Errorf("previous_response_id %q contains unsupported output item %q", previous, item["type"])
		}
	}
	chat["messages"] = messages
	return json.Marshal(chat)
}

// responseMessageToChat converts a Responses assistant message into the
// Chat message vocabulary used by the adapter. Responses output messages use
// output_text/refusal content parts, while Chat expects text/refusal fields.
// Unknown parts are rejected rather than silently dropping annotations or
// media from a previous_response_id chain.
func responseMessageToChat(item map[string]any) (map[string]any, error) {
	if item == nil {
		return nil, errors.New("message item is nil")
	}
	if role, ok := item["role"].(string); ok && strings.TrimSpace(role) != "" && role != "assistant" {
		return nil, fmt.Errorf("message role %q cannot be represented as assistant output", role)
	}
	content, refusal, err := responseMessageContentToChat(item["content"])
	if err != nil {
		return nil, err
	}
	message := map[string]any{"role": "assistant", "content": content}
	if refusal != "" {
		message["refusal"] = refusal
	}
	return message, nil
}

func responseMessageContentToChat(value any) (content any, refusal string, err error) {
	if value == nil {
		return nil, "", nil
	}
	if text, ok := value.(string); ok {
		return text, "", nil
	}
	parts, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]map[string]any); typedOK {
			parts = make([]any, len(typed))
			for index, part := range typed {
				parts[index] = part
			}
		} else {
			return nil, "", errors.New("message content must be a string or array")
		}
	}
	converted := make([]any, 0, len(parts))
	for index, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("message content part %d must be an object", index)
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "output_text", "text", "input_text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, "", fmt.Errorf("message content part %d text must be a string", index)
			}
			if annotations, exists := part["annotations"]; exists && !emptyJSONList(annotations) {
				return nil, "", fmt.Errorf("message content part %d annotations cannot be represented by Chat", index)
			}
			converted = append(converted, map[string]any{"type": "text", "text": text})
		case "refusal":
			text, ok := part["refusal"].(string)
			if !ok {
				return nil, "", fmt.Errorf("message refusal part %d must be a string", index)
			}
			if refusal != "" {
				return nil, "", errors.New("message contains multiple refusal parts")
			}
			refusal = text
		default:
			return nil, "", fmt.Errorf("message content part %d type %q cannot be represented by Chat", index, typ)
		}
	}
	return converted, refusal, nil
}

func emptyJSONList(value any) bool {
	switch list := value.(type) {
	case nil:
		return true
	case []any:
		return len(list) == 0
	case []map[string]any:
		return len(list) == 0
	default:
		return false
	}
}

func responseReasoningToChat(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]map[string]any); typedOK {
			parts = make([]any, len(typed))
			for index, part := range typed {
				parts[index] = part
			}
		} else {
			return "", errors.New("reasoning summary must be a string or summary_text array")
		}
	}
	var text strings.Builder
	for index, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("reasoning summary part %d must be an object", index)
		}
		typ, _ := part["type"].(string)
		if typ != "summary_text" && typ != "text" {
			return "", fmt.Errorf("reasoning summary part %d type %q cannot be represented by Chat", index, typ)
		}
		partText, ok := part["text"].(string)
		if !ok {
			return "", fmt.Errorf("reasoning summary part %d text must be a string", index)
		}
		text.WriteString(partText)
	}
	return text.String(), nil
}

func responseToolArgumentsToChat(value any) (string, error) {
	if value == nil {
		return "{}", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("tool arguments must be JSON: %w", err)
	}
	return string(data), nil
}

func responseToolOutputToChat(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("tool output must be JSON: %w", err)
	}
	return string(data), nil
}

func persistResponseAffinity(r *http.Request, st *store.Store, model, backend string, request, response []byte, status int) {
	if st == nil || (r.URL.Path != "/v1/responses" && r.URL.Path != "/v/responses") {
		return
	}
	var req map[string]any
	if json.Unmarshal(request, &req) != nil {
		return
	}
	if stored, ok := req["store"].(bool); ok && !stored {
		return
	}
	var out map[string]any
	storedResponse := append([]byte(nil), response...)
	if json.Unmarshal(response, &out) != nil {
		out = responseFromSSE(response, model)
		if canonical, marshalErr := json.Marshal(out); marshalErr == nil {
			storedResponse = canonical
		}
	}
	// Affinity is a restart/query aid, not a second unbounded response store.
	// A Chat adapter can produce a compact canonical JSON response whose size is
	// larger than the streaming capture budget (for example a large reasoning
	// or tool-argument chain). Do not persist that oversized representation even
	// though the client has already received the complete response.
	if len(storedResponse) > maxAffinityResponseBytes {
		return
	}
	id, _ := out["id"].(string)
	if id == "" {
		return
	}
	state := responseAffinityState(out, status)
	now := time.Now()
	ctx, cancel := affinityWriteContext()
	defer cancel()
	_ = st.UpsertResponseAffinity(ctx, store.ResponseAffinity{ResponseID: id, Model: model, Backend: backend, Status: state, Response: storedResponse, ExpiresAt: now.Add(30 * 24 * time.Hour), CreatedAt: now, UpdatedAt: now})
}

// responseFromSSE builds the small canonical response chain needed for
// previous_response_id when a Chat adapter was fed a streaming response. The
// client still receives the converted SSE; SQLite stores JSON only so GET and
// follow-up requests remain usable after a restart.
func responseFromSSE(body []byte, fallbackModel string) map[string]any {
	result := map[string]any{"object": "response", "model": fallbackModel, "status": "completed"}
	var completedResponse map[string]any
	textByIndex := make(map[int]*strings.Builder)
	reasoningByIndex := make(map[int]*strings.Builder)
	refusalByIndex := make(map[int]*strings.Builder)
	toolArgsByIndex := make(map[int]*strings.Builder)
	toolMetaByIndex := make(map[int]map[string]any)
	outputByIndex := make(map[int]map[string]any)
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if response, ok := event["response"].(map[string]any); ok {
			if value, ok := response["id"].(string); ok && value != "" {
				result["id"] = value
			}
			if value, ok := response["model"].(string); ok && value != "" {
				result["model"] = value
			}
			if usage, ok := response["usage"]; ok {
				result["usage"] = usage
			}
		}
		if usage, ok := event["usage"]; ok {
			result["usage"] = usage
		}
		typ, _ := event["type"].(string)
		index := responseSSEOutputIndex(event)
		switch typ {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				if textByIndex[index] == nil {
					textByIndex[index] = &strings.Builder{}
				}
				textByIndex[index].WriteString(delta)
			}
		case "response.output_text.done":
			if text, ok := event["text"].(string); ok {
				if textByIndex[index] == nil {
					textByIndex[index] = &strings.Builder{}
				}
				textByIndex[index].Reset()
				textByIndex[index].WriteString(text)
			}
		case "response.reasoning_summary_text.delta":
			if delta, ok := event["delta"].(string); ok {
				if reasoningByIndex[index] == nil {
					reasoningByIndex[index] = &strings.Builder{}
				}
				reasoningByIndex[index].WriteString(delta)
			}
		case "response.reasoning_summary_text.done":
			if text, ok := event["text"].(string); ok {
				if reasoningByIndex[index] == nil {
					reasoningByIndex[index] = &strings.Builder{}
				}
				reasoningByIndex[index].Reset()
				reasoningByIndex[index].WriteString(text)
			}
		case "response.refusal.delta":
			if delta, ok := event["delta"].(string); ok {
				if refusalByIndex[index] == nil {
					refusalByIndex[index] = &strings.Builder{}
				}
				refusalByIndex[index].WriteString(delta)
			}
		case "response.refusal.done":
			if refusal, ok := event["refusal"].(string); ok {
				if refusalByIndex[index] == nil {
					refusalByIndex[index] = &strings.Builder{}
				}
				refusalByIndex[index].Reset()
				refusalByIndex[index].WriteString(refusal)
			}
		case "response.function_call_arguments.delta":
			if delta, ok := event["delta"].(string); ok {
				if toolArgsByIndex[index] == nil {
					toolArgsByIndex[index] = &strings.Builder{}
				}
				toolArgsByIndex[index].WriteString(delta)
			}
			responseSSEToolMetadata(toolMetaByIndex, index, event)
		case "response.function_call_arguments.done":
			if arguments, ok := event["arguments"].(string); ok {
				if toolArgsByIndex[index] == nil {
					toolArgsByIndex[index] = &strings.Builder{}
				}
				toolArgsByIndex[index].Reset()
				toolArgsByIndex[index].WriteString(arguments)
			}
			responseSSEToolMetadata(toolMetaByIndex, index, event)
		case "response.output_item.added", "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok {
				outputByIndex[index] = item
			}
		case "response.completed", "response.incomplete", "response.cancelled":
			if response, ok := event["response"].(map[string]any); ok {
				completedResponse = response
				if usage, ok := response["usage"]; ok {
					result["usage"] = usage
				}
			}
		case "response.failed":
			if response, ok := event["response"].(map[string]any); ok {
				completedResponse = response
				result["status"] = "failed"
			}
		}
	}
	if completedResponse != nil {
		for key, value := range completedResponse {
			result[key] = value
		}
		if _, ok := result["model"]; !ok || result["model"] == "" {
			result["model"] = fallbackModel
		}
		if _, ok := result["output"]; ok {
			return result
		}
	}
	// Older/native-compatible servers may terminate with [DONE] without a
	// response.completed event. Reconstruct every output item observed in the
	// lifecycle so GET and previous_response_id retain reasoning, refusal and
	// tool calls instead of silently degrading to text-only history.
	for index := range textByIndex {
		if _, ok := outputByIndex[index]; !ok {
			outputByIndex[index] = map[string]any{"type": "message", "role": "assistant"}
		}
	}
	for index := range reasoningByIndex {
		if _, ok := outputByIndex[index]; !ok {
			outputByIndex[index] = map[string]any{"type": "reasoning"}
		}
	}
	for index := range refusalByIndex {
		if _, ok := outputByIndex[index]; !ok {
			outputByIndex[index] = map[string]any{"type": "message", "role": "assistant"}
		}
	}
	for index := range toolArgsByIndex {
		if _, ok := outputByIndex[index]; !ok {
			outputByIndex[index] = map[string]any{"type": "function_call"}
		}
	}
	for index, item := range outputByIndex {
		if item == nil {
			continue
		}
		switch itemType, _ := item["type"].(string); itemType {
		case "message":
			if text := builderString(textByIndex[index]); text != "" {
				item["content"] = []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}
			} else if refusal := builderString(refusalByIndex[index]); refusal != "" {
				item["content"] = []any{map[string]any{"type": "refusal", "refusal": refusal}}
			}
		case "reasoning":
			if summary := builderString(reasoningByIndex[index]); summary != "" {
				item["summary"] = []any{map[string]any{"type": "summary_text", "text": summary}}
			}
		case "function_call":
			if meta := toolMetaByIndex[index]; meta != nil {
				for key, value := range meta {
					item[key] = value
				}
			}
			if arguments := builderString(toolArgsByIndex[index]); arguments != "" {
				item["arguments"] = arguments
			}
		}
	}
	indices := make([]int, 0, len(outputByIndex))
	for index := range outputByIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]any, 0, len(indices))
	for _, index := range indices {
		if item := outputByIndex[index]; item != nil {
			output = append(output, item)
		}
	}
	result["output"] = output
	return result
}

func responseSSEOutputIndex(event map[string]any) int {
	value, ok := event["output_index"].(float64)
	maxInt := float64(int(^uint(0) >> 1))
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) || value >= maxInt {
		return 0
	}
	return int(value)
}

func responseSSEToolMetadata(metadata map[int]map[string]any, index int, event map[string]any) {
	current := metadata[index]
	if current == nil {
		current = make(map[string]any)
		metadata[index] = current
	}
	for _, key := range []string{"call_id", "name", "item_id"} {
		if value, ok := event[key].(string); ok && value != "" {
			if key == "item_id" {
				current["id"] = value
				continue
			}
			current[key] = value
		}
	}
}

func builderString(builder *strings.Builder) string {
	if builder == nil {
		return ""
	}
	return builder.String()
}

type bufferedResponseWriter struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}
func (w *bufferedResponseWriter) Header() http.Header { return w.header }
func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(data) > backendTransformBodyLimit {
		w.overflow = true
		return 0, fmt.Errorf("buffered response body exceeds %d bytes", backendTransformBodyLimit)
	}
	return w.body.Write(data)
}
func (w *bufferedResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func clearTransformedBodyHeaders(header http.Header) {
	for _, key := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Transfer-Encoding", "Trailer"} {
		header.Del(key)
	}
}
