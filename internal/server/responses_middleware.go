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

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/responses"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func CreateResponsesAdapterMiddleware(cfg config.Config, st *store.Store) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || (r.URL.Path != "/v1/responses" && r.URL.Path != "/v/responses") {
				next.ServeHTTP(w, r)
				return
			}
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}
			modelConfig, ok := cfg.Models[data.ModelID]
			if !ok || modelConfig.Backend.EffectiveProtocol() != config.AdapterProtocolResponsesToChat {
				// Native Responses is a strict pass-through; affinity is recorded
				// by the backend response recorder only when an id is available.
				next.ServeHTTP(w, r)
				return
			}
			requestBody, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
				return
			}
			toolContext := responses.NewToolContextFromRequestBytes(requestBody)
			chatBody, err := responses.ResponsesToChat(requestBody)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			chatBody, err = addPreviousResponseChainForModel(r.Context(), chatBody, requestBody, data.ModelID, st)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(chatBody))
			r.Header.Set("Content-Length", fmt.Sprintf("%d", len(chatBody)))
			r.ContentLength = int64(len(chatBody))
			// Keep the public Responses path visible to metrics, filters and
			// affinity persistence. The terminal model dispatcher consumes this
			// marker and rewrites only the outbound backend path to Chat.
			r = markResponsesChatRequest(r)
			if responsesStreamRequested(requestBody) {
				streamWriter := newResponsesStreamWriter(w, r, modelConfig.Backend.Type, data.ModelID, requestBody, toolContext, st)
				next.ServeHTTP(streamWriter, r)
				if finishErr := streamWriter.finish(); finishErr != nil {
					// A streaming response may already have committed bytes. In that
					// case the only safe action is to close the stream; attempting to
					// write a second JSON error would corrupt the protocol. Before the
					// first byte, surface a normal gateway error instead.
					if !streamWriter.committed {
						swaputil.SendResponse(w, r, http.StatusBadGateway, finishErr.Error())
					}
				}
				return
			}
			buffer := newBufferedResponseWriter()
			next.ServeHTTP(buffer, r)
			if buffer.overflow {
				swaputil.SendResponse(w, r, http.StatusBadGateway, fmt.Sprintf("responses response body exceeds %d bytes", backendTransformBodyLimit))
				return
			}
			if buffer.status == 0 {
				buffer.status = http.StatusOK
			}
			converted := append([]byte(nil), buffer.body.Bytes()...)
			transformed := false
			if buffer.status >= 200 && buffer.status < 300 {
				if strings.Contains(strings.ToLower(buffer.Header().Get("Content-Type")), "text/event-stream") {
					converted, err = responses.ConvertSSE(converted)
					if err == nil {
						buffer.Header().Set("Content-Type", "text/event-stream")
						transformed = true
					}
				} else if len(converted) > 0 {
					converted, err = responses.ChatResponseToResponsesWithContext(converted, toolContext)
					if err == nil {
						buffer.Header().Set("Content-Type", "application/json")
						transformed = true
					}
				}
			} else {
				// Upstream Chat error bodies do not speak the Responses error
				// vocabulary (MiniMax base_resp, custom detail fields, plain
				// text). Normalize them so Responses clients can recognize
				// the failure; the upstream status code is preserved.
				converted = responses.ChatErrorToResponseError(converted)
				buffer.Header().Set("Content-Type", "application/json")
				transformed = true
			}
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
				return
			}
			if transformed {
				// The adapter changes the representation and therefore the byte
				// length (and possibly compression). Do not forward body metadata
				// calculated for the upstream Chat payload; stale values make the
				// net/http server truncate or mis-decode the Responses body.
				clearTransformedBodyHeaders(buffer.Header())
			}
			copyHeaders(w.Header(), buffer.Header())
			w.WriteHeader(buffer.status)
			_, _ = w.Write(converted)
			persistResponseAffinity(r, st, data.ModelID, modelConfig.Backend.Type, requestBody, converted, buffer.status)
		})
	}
}

type responsesChatBackendPathKey struct{}

// markResponsesChatRequest carries the outbound Chat path without changing
// the public request URL. Keeping this as request context state means metrics,
// filters and audit continue to observe /v1/responses while only the terminal
// local/peer dispatcher presents /v1/chat/completions to a Chat-only backend.
func markResponsesChatRequest(r *http.Request) *http.Request {
	if r == nil || r.URL == nil {
		return r
	}
	chatPath := "/v1/chat/completions"
	if r.URL.Path == "/v/responses" {
		chatPath = "/v/chat/completions"
	}
	return r.WithContext(context.WithValue(r.Context(), responsesChatBackendPathKey{}, chatPath))
}

// rewriteResponsesChatBackendPath applies the marker immediately around the
// terminal local/peer router call. It returns a restore function so request
// URL state never leaks back to outer middleware or response affinity code.
func rewriteResponsesChatBackendPath(r *http.Request) func() {
	if r == nil || r.URL == nil {
		return func() {}
	}
	chatPath, _ := r.Context().Value(responsesChatBackendPathKey{}).(string)
	if strings.TrimSpace(chatPath) == "" {
		return func() {}
	}
	originalPath, originalRawPath, originalRequestURI := r.URL.Path, r.URL.RawPath, r.RequestURI
	r.URL.Path = chatPath
	r.URL.RawPath = ""
	if originalRequestURI != "" {
		r.RequestURI = r.URL.RequestURI()
	}
	return func() {
		r.URL.Path = originalPath
		r.URL.RawPath = originalRawPath
		r.RequestURI = originalRequestURI
	}
}

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

func responsesStreamRequested(body []byte) bool {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	stream, _ := request["stream"].(bool)
	return stream
}

// responsesStreamWriter converts Chat SSE incrementally while retaining a
// bounded copy of the converted stream for response affinity. It deliberately
// forwards non-2xx bodies unchanged: an upstream error is already in the
// canonical error format and should not be wrapped as a successful Responses
// lifecycle.
type responsesStreamWriter struct {
	writer      http.ResponseWriter
	request     *http.Request
	backend     string
	model       string
	requestBody []byte
	store       *store.Store
	converter   *responses.SSEConverter
	captured    bytes.Buffer
	status      int
	committed   bool
	stream      bool
	truncated   bool
	finished    bool
	// Non-2xx upstream bodies are buffered (bounded) so finish() can emit
	// them in the Responses error vocabulary instead of forwarding a body
	// the client cannot parse.
	errorBody        bytes.Buffer
	errorPassthrough bool
}

func newResponsesStreamWriter(w http.ResponseWriter, r *http.Request, backend, model string, requestBody []byte, toolContext *responses.ToolContext, st *store.Store) *responsesStreamWriter {
	return &responsesStreamWriter{
		writer:      w,
		request:     r,
		backend:     backend,
		model:       model,
		requestBody: append([]byte(nil), requestBody...),
		store:       st,
		converter:   responses.NewSSEConverterWithContext(toolContext),
		stream:      true,
	}
}

func (w *responsesStreamWriter) Header() http.Header { return w.writer.Header() }

func (w *responsesStreamWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status >= 200 && status < 300 {
		w.prepareStreamHeaders()
		w.writer.WriteHeader(status)
		w.committed = true
		return
	}
	// Non-2xx: defer the header so finish() can rewrite the buffered body.
}

func (w *responsesStreamWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if len(data) == 0 {
		return 0, nil
	}
	if w.status < 200 || w.status >= 300 {
		if w.errorPassthrough {
			return w.writer.Write(data)
		}
		if w.errorBody.Len()+len(data) > backendTransformBodyLimit {
			// Body too large to buffer: fall back to a raw passthrough.
			w.errorPassthrough = true
			w.writer.WriteHeader(w.status)
			w.committed = true
			_, _ = w.writer.Write(w.errorBody.Bytes())
			w.errorBody.Reset()
			return w.writer.Write(data)
		}
		w.errorBody.Write(data)
		return len(data), nil
	}
	converted, err := w.converter.Convert(data)
	if err != nil {
		return 0, err
	}
	w.capture(converted)
	if len(converted) == 0 {
		return len(data), nil
	}
	if _, err := w.writer.Write(converted); err != nil {
		return 0, err
	}
	w.flush()
	return len(data), nil
}

func (w *responsesStreamWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flush()
}

func (w *responsesStreamWriter) flush() {
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesStreamWriter) prepareStreamHeaders() {
	header := w.writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Del("Content-Length")
	header.Del("Content-Encoding")
	header.Del("Content-Range")
	header.Del("Transfer-Encoding")
	header.Del("Trailer")
}

func (w *responsesStreamWriter) capture(data []byte) {
	if w.truncated || len(data) == 0 {
		return
	}
	remaining := maxAffinityResponseBytes - w.captured.Len()
	if remaining <= 0 {
		w.truncated = true
		return
	}
	if len(data) > remaining {
		_, _ = w.captured.Write(data[:remaining])
		w.truncated = true
		return
	}
	_, _ = w.captured.Write(data)
}

func (w *responsesStreamWriter) finish() error {
	if w.finished {
		return nil
	}
	w.finished = true
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status < 200 || w.status >= 300 {
		if w.errorPassthrough || w.committed {
			return nil
		}
		// Emit the buffered upstream error in the Responses error vocabulary
		// while keeping the upstream status code.
		converted := responses.ChatErrorToResponseError(w.errorBody.Bytes())
		header := w.writer.Header()
		header.Set("Content-Type", "application/json")
		clearTransformedBodyHeaders(header)
		w.writer.WriteHeader(w.status)
		w.committed = true
		_, _ = w.writer.Write(converted)
		return nil
	}
	converted, err := w.converter.Flush()
	if err != nil {
		return err
	}
	w.capture(converted)
	if len(converted) > 0 {
		if _, err := w.writer.Write(converted); err != nil {
			return err
		}
		w.flush()
	}
	if !w.truncated {
		// Persist the converter's canonical value rather than reconstructing
		// a response by scanning output_text deltas. The latter loses
		// reasoning, refusal, tool calls and their usage when a stored chain
		// is replayed after a restart.
		stored := w.captured.Bytes()
		if canonical := w.converter.CanonicalResponse(); canonical != nil {
			if data, marshalErr := json.Marshal(canonical); marshalErr == nil {
				stored = data
			}
		}
		persistResponseAffinity(w.request, w.store, w.model, w.backend, w.requestBody, stored, w.status)
	}
	return nil
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
