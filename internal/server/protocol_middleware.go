package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type protocolBackendPathKey struct{}

type protocolBackendPath struct {
	Source protocolrelaykit.Format
	Path   string
	Format protocolrelaykit.Format
	Stream bool
}

func protocolTargetFormat(r *http.Request) protocolrelaykit.Format {
	if r == nil {
		return ""
	}
	target, _ := r.Context().Value(protocolBackendPathKey{}).(protocolBackendPath)
	return target.Format
}

func prepareGeminiRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := protocolrelaykit.RouteForRequest(r.Method, r.URL.Path)
		if !ok || route.Format != protocolrelaykit.Gemini {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "unsupported Gemini generateContent action")
			return
		}
		r.SetPathValue("model", route.Model)
		r.SetPathValue("geminiPath", route.Model+":"+route.Action)
		next.ServeHTTP(w, r)
	})
}

func CreateProtocolAdapterMiddleware(cfg config.Config, st *store.Store) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, ok := protocolrelaykit.RouteForRequest(r.Method, r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}
			modelConfig, exists := cfg.Models[data.ModelID]
			if !exists {
				next.ServeHTTP(w, r)
				return
			}
			if route.Format == protocolrelaykit.OpenAIChat {
				requestBody, err := readResponsesRequestBody(r)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
					return
				}
				requestBody, _, err = protocolrelaykit.StripHostedSearchTools(requestBody, route.Format)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
				if err := setProtocolRequestBody(r, requestBody); err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
			}
			target, enabled := protocolTargetFor(route.Format, modelConfig.Backend.Protocol)
			if !enabled || target == route.Format {
				next.ServeHTTP(w, r)
				return
			}

			requestBody, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
				return
			}
			stream := protocolrelaykit.RequestStreamed(route, requestBody)
			meta := protocolrelaykit.NewMeta(data.ModelID, stream)
			converted, err := convertProtocolRequest(r.Context(), meta, route.Format, target, requestBody, data.ModelID, st)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			path, err := protocolrelaykit.TargetPathForRequest(target, data.ModelID, stream, r.URL.Path)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			if err := setProtocolRequestBody(r, converted); err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			r = markProtocolBackendRequest(r, route.Format, target, path, stream)
			if route.Format == protocolrelaykit.Gemini {
				r.Header.Del("x-goog-api-key")
			}
			if stream {
				writer, err := newProtocolStreamWriter(w, r, st, data.ModelID, modelConfig.Backend.Type, target, route.Format, requestBody, meta)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
				next.ServeHTTP(writer, r)
				if finishErr := writer.finish(); finishErr != nil && !writer.committed {
					swaputil.SendResponse(w, r, http.StatusBadGateway, finishErr.Error())
				}
				return
			}

			buffer := newBufferedResponseWriter()
			next.ServeHTTP(buffer, r)
			if buffer.overflow {
				swaputil.SendResponse(w, r, http.StatusBadGateway, fmt.Sprintf("protocol response body exceeds %d bytes", backendTransformBodyLimit))
				return
			}
			if buffer.status == 0 {
				buffer.status = http.StatusOK
			}
			body := append([]byte(nil), buffer.body.Bytes()...)
			transformed := false
			if buffer.status >= 200 && buffer.status < 300 {
				if len(bytes.TrimSpace(body)) != 0 {
					result, convertErr := protocolrelaykit.ConvertResponse(r.Context(), meta, target, route.Format, body)
					if convertErr != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, convertErr.Error())
						return
					}
					logResponseConversion(result, data.ModelID)
					body, err = protocolrelaykit.Marshal(result.Value)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					buffer.Header().Set("Content-Type", "application/json")
					transformed = true
				}
			} else {
				body = normalizeProtocolError(body, route.Format, buffer.status)
				buffer.Header().Set("Content-Type", "application/json")
				transformed = true
			}
			if transformed {
				clearTransformedBodyHeaders(buffer.Header())
			}
			copyHeaders(w.Header(), buffer.Header())
			w.WriteHeader(buffer.status)
			_, _ = w.Write(body)
			if route.Format == protocolrelaykit.OpenAIResponses && buffer.status >= 200 && buffer.status < 300 {
				persistResponseAffinity(r, st, data.ModelID, modelConfig.Backend.Type, requestBody, body, buffer.status)
			}
		})
	}
}

func protocolTargetFor(source protocolrelaykit.Format, _ string) (protocolrelaykit.Format, bool) {
	switch source {
	case protocolrelaykit.OpenAIChat, protocolrelaykit.OpenAIResponses, protocolrelaykit.ClaudeMessages, protocolrelaykit.Gemini:
		return protocolrelaykit.OpenAIChat, source != protocolrelaykit.OpenAIChat
	default:
		return "", false
	}
}

func convertProtocolRequest(ctx context.Context, meta *convmeta.Values, source, target protocolrelaykit.Format, body []byte, model string, st *store.Store) ([]byte, error) {
	convertBody := body
	if source == protocolrelaykit.OpenAIResponses {
		var err error
		convertBody, err = withoutPreviousResponseID(body)
		if err != nil {
			return nil, err
		}
	}
	if source == protocolrelaykit.OpenAIResponses && target != protocolrelaykit.OpenAIChat {
		chat, err := protocolrelaykit.ConvertRequest(ctx, meta, source, protocolrelaykit.OpenAIChat, convertBody, model)
		if err != nil {
			return nil, err
		}
		logRequestConversion(chat, model)
		chatBody, err := protocolrelaykit.Marshal(chat.Value)
		if err != nil {
			return nil, err
		}
		chatBody, err = addPreviousResponseChainForModel(ctx, chatBody, body, model, st)
		if err != nil {
			return nil, err
		}
		chat, err = protocolrelaykit.ConvertRequest(ctx, meta, protocolrelaykit.OpenAIChat, target, chatBody, model)
		if err != nil {
			return nil, err
		}
		logRequestConversion(chat, model)
		return protocolrelaykit.Marshal(chat.Value)
	}
	result, err := protocolrelaykit.ConvertRequest(ctx, meta, source, target, convertBody, model)
	if err != nil {
		return nil, err
	}
	logRequestConversion(result, model)
	converted, err := protocolrelaykit.Marshal(result.Value)
	if err != nil {
		return nil, err
	}
	if source == protocolrelaykit.OpenAIResponses && target == protocolrelaykit.OpenAIChat {
		return addPreviousResponseChainForModel(ctx, converted, body, model, st)
	}
	return converted, nil
}

func withoutPreviousResponseID(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode Responses request: %w", err)
	}
	delete(request, "previous_response_id")
	return json.Marshal(request)
}

func setProtocolRequestBody(r *http.Request, body []byte) error {
	if r == nil {
		return fmt.Errorf("request is required")
	}
	setBackendRequestBody(r, body)
	r.Header.Set("Content-Type", "application/json")
	return nil
}

func markProtocolBackendRequest(r *http.Request, source, target protocolrelaykit.Format, path string, stream bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), protocolBackendPathKey{}, protocolBackendPath{Source: source, Path: path, Format: target, Stream: stream}))
}

func rewriteProtocolBackendPath(r *http.Request) func() {
	if r == nil || r.URL == nil {
		return func() {}
	}
	backend, ok := r.Context().Value(protocolBackendPathKey{}).(protocolBackendPath)
	if !ok || strings.TrimSpace(backend.Path) == "" {
		return func() {}
	}
	originalPath, originalRawPath, originalRequestURI, originalQuery := r.URL.Path, r.URL.RawPath, r.RequestURI, r.URL.RawQuery
	r.URL.Path = backend.Path
	r.URL.RawPath = ""
	if backend.Format == protocolrelaykit.Gemini {
		query := r.URL.Query()
		if backend.Stream {
			query.Set("alt", "sse")
		} else if strings.EqualFold(query.Get("alt"), "sse") {
			query.Del("alt")
		}
		r.URL.RawQuery = query.Encode()
	} else if backend.Source == protocolrelaykit.Gemini {
		query := r.URL.Query()
		query.Del("alt")
		r.URL.RawQuery = query.Encode()
	}
	if originalRequestURI != "" {
		r.RequestURI = r.URL.RequestURI()
	}
	return func() {
		r.URL.Path = originalPath
		r.URL.RawPath = originalRawPath
		r.URL.RawQuery = originalQuery
		r.RequestURI = originalRequestURI
	}
}

func logRequestConversion(result *relayconvert.RequestResult, model string) {
	if result == nil {
		return
	}
	slog.Debug("protocol request converted", "model", model, "from", result.From, "to", result.To, "converter", result.Converter, "quality", result.Quality)
	if len(result.Diagnostics) > 0 {
		slog.Warn("protocol request conversion diagnostics", "model", model, "diagnostics", result.Diagnostics)
	}
}

func logResponseConversion(result *relayconvert.ResponseResult, model string) {
	if result == nil {
		return
	}
	slog.Debug("protocol response converted", "model", model, "from", result.From, "to", result.To, "converter", result.Converter, "quality", result.Quality)
	if len(result.Diagnostics) > 0 {
		slog.Warn("protocol response conversion diagnostics", "model", model, "diagnostics", result.Diagnostics)
	}
}

func normalizeProtocolError(body []byte, target protocolrelaykit.Format, status int) []byte {
	var value any
	message := strings.TrimSpace(string(body))
	code := "upstream_error"
	if json.Unmarshal(body, &value) == nil {
		if object, ok := value.(map[string]any); ok {
			if nested, ok := object["error"].(map[string]any); ok {
				if text, ok := nested["message"].(string); ok && strings.TrimSpace(text) != "" {
					message = text
				}
				if text, ok := nested["type"].(string); ok && text != "" {
					code = text
				}
			} else if text, ok := object["message"].(string); ok && strings.TrimSpace(text) != "" {
				message = text
			} else if base, ok := object["base_resp"].(map[string]any); ok {
				if text, ok := base["status_msg"].(string); ok && strings.TrimSpace(text) != "" {
					message = text
				}
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	var normalized any
	switch target {
	case protocolrelaykit.ClaudeMessages:
		normalized = map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}
	case protocolrelaykit.Gemini:
		normalized = map[string]any{"error": map[string]any{"code": status, "message": message, "status": "UNKNOWN"}}
	default:
		normalized = map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}}
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return []byte(`{"error":{"message":"upstream request failed"}}`)
	}
	return encoded
}

type protocolStreamWriter struct {
	writer        http.ResponseWriter
	request       *http.Request
	store         *store.Store
	model         string
	backend       string
	upstream      protocolrelaykit.Format
	client        protocolrelaykit.Format
	requestBody   []byte
	meta          *convmeta.Values
	state         *relayconvert.ResponseStreamState
	streamOptions relayconvert.ResponseStreamOptions
	pending       bytes.Buffer
	captured      bytes.Buffer
	canonical     []byte
	errorBody     bytes.Buffer
	status        int
	committed     bool
	errorForward  bool
	streamErr     error
	finished      bool
}

func newProtocolStreamWriter(w http.ResponseWriter, r *http.Request, st *store.Store, model, backend string, upstream, client protocolrelaykit.Format, requestBody []byte, meta *convmeta.Values) (*protocolStreamWriter, error) {
	options := relayconvert.ResponseStreamOptions{
		Model: model, Created: time.Now().Unix(), IncludeUsage: true, EmitSequenceNumber: true,
	}
	var state *relayconvert.ResponseStreamState
	if upstream != protocolrelaykit.OpenAIChat || client != protocolrelaykit.OpenAIResponses {
		var err error
		state, err = relayconvert.NewResponseStreamState(upstream, client, options)
		if err != nil {
			return nil, err
		}
	}
	return &protocolStreamWriter{
		writer: w, request: r, store: st, model: model, backend: backend,
		upstream: upstream, client: client, requestBody: append([]byte(nil), requestBody...),
		meta: meta, state: state, streamOptions: options,
	}, nil
}

func (w *protocolStreamWriter) Header() http.Header { return w.writer.Header() }

func (w *protocolStreamWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status < 200 || status >= 300 {
		return
	}
	w.prepareHeaders()
	w.writer.WriteHeader(status)
	w.committed = true
}

func (w *protocolStreamWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if len(data) == 0 {
		return 0, nil
	}
	if w.status < 200 || w.status >= 300 {
		if w.errorForward {
			return w.writer.Write(data)
		}
		if w.errorBody.Len()+len(data) > backendTransformBodyLimit {
			w.errorForward = true
			w.writer.WriteHeader(w.status)
			w.committed = true
			_, _ = w.writer.Write(w.errorBody.Bytes())
			w.errorBody.Reset()
			return w.writer.Write(data)
		}
		_, _ = w.errorBody.Write(data)
		return len(data), nil
	}
	if _, err := w.pending.Write(data); err != nil {
		return 0, err
	}
	for {
		event, ok := takeSSEEvent(&w.pending)
		if !ok {
			break
		}
		if err := w.convertEvent(event); err != nil {
			w.streamErr = err
			return 0, err
		}
	}
	return len(data), nil
}

func (w *protocolStreamWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flush()
}

func takeSSEEvent(buffer *bytes.Buffer) ([]byte, bool) {
	data := buffer.Bytes()
	index := bytes.Index(data, []byte("\n\n"))
	separatorLength := 2
	if index < 0 {
		index = bytes.Index(data, []byte("\r\n\r\n"))
		separatorLength = 4
	}
	if index < 0 {
		return nil, false
	}
	event := append([]byte(nil), data[:index]...)
	buffer.Next(index + separatorLength)
	return event, true
}

func sseData(event []byte) (string, bool) {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(event), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

func (w *protocolStreamWriter) convertEvent(event []byte) error {
	data, ok := sseData(event)
	if !ok || data == "" || data == "[DONE]" {
		return nil
	}
	response, err := protocolrelaykit.DecodeStreamResponse(w.upstream, []byte(data))
	if err != nil {
		return err
	}
	if w.state == nil {
		if w.client == protocolrelaykit.OpenAIResponses {
			w.streamOptions.ID = protocolrelaykit.StreamResponseID(w.upstream, response)
		}
		w.state, err = relayconvert.NewResponseStreamState(w.upstream, w.client, w.streamOptions)
		if err != nil {
			return err
		}
	}
	results, err := relayconvert.ConvertStreamResponseChunk(w.request.Context(), w.meta, w.state, response)
	if err != nil {
		return err
	}
	return w.writeResults(results)
}

func (w *protocolStreamWriter) writeResults(results []relayconvert.ResponseResult) error {
	for _, result := range results {
		data, err := protocolrelaykit.EncodeSSE(w.client, result.Value)
		if err != nil {
			return err
		}
		w.capture(data)
		w.captureCompletedResponse(data)
		if _, err := w.writer.Write(data); err != nil {
			return err
		}
		w.flush()
	}
	return nil
}

func (w *protocolStreamWriter) capture(data []byte) {
	if w.captured.Len() >= maxAffinityResponseBytes {
		return
	}
	remaining := maxAffinityResponseBytes - w.captured.Len()
	if len(data) > remaining {
		_, _ = w.captured.Write(data[:remaining])
		return
	}
	_, _ = w.captured.Write(data)
}

func (w *protocolStreamWriter) captureCompletedResponse(data []byte) {
	if w.client != protocolrelaykit.OpenAIResponses {
		return
	}
	for _, rawLine := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var envelope map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &envelope) != nil {
			continue
		}
		if response, exists := envelope["response"]; exists && len(response) > 0 && string(response) != "null" {
			w.canonical = append(w.canonical[:0], response...)
		}
	}
}

func (w *protocolStreamWriter) prepareHeaders() {
	header := w.writer.Header()
	header.Set("Content-Type", "text/event-stream")
	for _, key := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Transfer-Encoding", "Trailer"} {
		header.Del(key)
	}
}

func (w *protocolStreamWriter) flush() {
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *protocolStreamWriter) finish() error {
	if w.finished {
		return nil
	}
	w.finished = true
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status < 200 || w.status >= 300 {
		if w.errorForward || w.committed {
			return nil
		}
		header := w.writer.Header()
		header.Set("Content-Type", "application/json")
		clearTransformedBodyHeaders(header)
		w.writer.WriteHeader(w.status)
		w.committed = true
		_, _ = w.writer.Write(normalizeProtocolError(w.errorBody.Bytes(), w.client, w.status))
		return nil
	}
	if w.streamErr != nil {
		return w.streamErr
	}
	if w.pending.Len() > 0 {
		pending := append([]byte(nil), w.pending.Bytes()...)
		w.pending.Reset()
		if err := w.convertEvent(pending); err != nil {
			return err
		}
	}
	if w.state == nil {
		state, err := relayconvert.NewResponseStreamState(w.upstream, w.client, w.streamOptions)
		if err != nil {
			return err
		}
		w.state = state
	}
	results, err := relayconvert.FinalizeStreamResponse(w.request.Context(), w.meta, w.state)
	if err != nil {
		return err
	}
	if err := w.writeResults(results); err != nil {
		return err
	}
	if done := protocolrelaykit.DoneSSE(w.client); len(done) > 0 {
		w.capture(done)
		if _, err := w.writer.Write(done); err != nil {
			return err
		}
		w.flush()
	}
	if w.client == protocolrelaykit.OpenAIResponses {
		stored := w.canonical
		if len(stored) == 0 {
			stored = w.captured.Bytes()
		}
		persistResponseAffinity(w.request, w.store, w.model, w.backend, w.requestBody, stored, w.status)
	}
	if diagnostics := w.state.Diagnostics(); len(diagnostics) > 0 {
		slog.Warn("protocol stream conversion diagnostics", "model", w.model, "diagnostics", diagnostics)
	}
	return nil
}
