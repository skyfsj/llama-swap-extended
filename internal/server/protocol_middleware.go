package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

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

// ProviderDialect describes the upstream surface a provider-routed model is
// served by, so the conversion layer applies to provider forwards too.
type ProviderDialect struct {
	// Type is the adapter's API surface: openai (Completions),
	// openai-responses, anthropic or gemini.
	Type string
	// Label names the adapter for logging and response-affinity attribution.
	Label string
	// Surfaces is how many adapters of the provider expose this model. When it
	// is more than one, the forward path converts per surface and falls back to
	// another surface if an upstream rejects the request.
	Surfaces int
}

// ProviderDialectResolver reports the upstream dialect for a model ID the
// provider gateway serves; ok=false means the model is not provider-routed.
type ProviderDialectResolver func(modelID string) (ProviderDialect, bool)

func CreateProtocolAdapterMiddleware(cfg config.Config, st *store.Store, providerDialect ProviderDialectResolver) chain.Middleware {
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
			modelConfig, isLocal := cfg.Models[data.ModelID]
			// Client-side normalization happens before any surface decision:
			// the multi-surface path below converts this body per surface.
			if route.Format == protocolrelaykit.OpenAIChat {
				requestBody, err := readResponsesRequestBody(r)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
					return
				}
				// An extension that injected provider-hosted search tools did so
				// deliberately: stripping them here would make the preset a
				// no-op. Only a client-sent hosted tool is stripped.
				if !requestHasExtensionServerTools(r) {
					requestBody, _, err = protocolrelaykit.StripHostedSearchTools(requestBody, route.Format)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
				}
				if err := setProtocolRequestBody(r, requestBody); err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
			}
			if !isLocal && providerDialect != nil {
				if resolved, ok := providerDialect(data.ModelID); ok && resolved.Surfaces > 1 {
					// The model is exposed on several upstream surfaces: the
					// forward path converts per surface and falls back to the
					// next one when an upstream rejects it, so it owns the
					// conversion for this request.
					next.ServeHTTP(w, r)
					return
				}
			}
			var provider ProviderDialect
			if !isLocal {
				// Provider-routed models use the same conversion layer: the
				// adapter declares which dialect its upstream speaks.
				if providerDialect == nil {
					next.ServeHTTP(w, r)
					return
				}
				resolved, ok := providerDialect(data.ModelID)
				if !ok {
					next.ServeHTTP(w, r)
					return
				}
				provider = resolved
			}
			target, enabled := protocolTargetFor(route.Format, modelConfig.Backend.Protocol)
			if !isLocal {
				target, enabled = providerTargetFor(route.Format, provider)
			}
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
			if target == protocolrelaykit.Gemini {
				// Make the converted request indistinguishable from a native
				// Gemini call: the model lives in the path, and the model
				// rewrite downstream (upstream name) reads it from there.
				if targetRoute, ok := protocolrelaykit.RouteForRequest(r.Method, path); ok {
					r.SetPathValue("model", targetRoute.Model)
					r.SetPathValue("geminiPath", targetRoute.Model+":"+targetRoute.Action)
				}
			}
			if route.Format == protocolrelaykit.Gemini {
				r.Header.Del("x-goog-api-key")
			}
			backendLabel := modelConfig.Backend.Type
			if !isLocal {
				backendLabel = provider.Label
			}
			writer, err := newProtocolResponseWriter(w, r, st, data.ModelID, backendLabel, target, route.Format, requestBody, meta, stream)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			next.ServeHTTP(writer, r)
			if finishErr := writer.finish(); finishErr != nil && !writer.committed() {
				swaputil.SendResponse(w, r, http.StatusBadGateway, finishErr.Error())
				return
			}
		})
	}
}

// surfaceTypeForFormat maps a client dialect onto the provider type (API
// surface) that serves it natively. Empty means the dialect has no surface.
func surfaceTypeForFormat(format protocolrelaykit.Format) string {
	switch format {
	case protocolrelaykit.OpenAIChat:
		return config.ProviderTypeOpenAI
	case protocolrelaykit.OpenAIResponses:
		return config.ProviderTypeOpenAIResponses
	case protocolrelaykit.ClaudeMessages:
		return config.ProviderTypeAnthropic
	case protocolrelaykit.Gemini:
		return config.ProviderTypeGemini
	case protocolrelaykit.GeminiInteractions:
		return config.ProviderTypeGeminiInteractions
	default:
		return ""
	}
}

// surfaceFormatForType is the inverse: the dialect a provider type speaks.
func surfaceFormatForType(surface string) protocolrelaykit.Format {
	switch surface {
	case config.ProviderTypeOpenAI:
		return protocolrelaykit.OpenAIChat
	case config.ProviderTypeOpenAIResponses:
		return protocolrelaykit.OpenAIResponses
	case config.ProviderTypeAnthropic:
		return protocolrelaykit.ClaudeMessages
	case config.ProviderTypeGemini:
		return protocolrelaykit.Gemini
	case config.ProviderTypeGeminiInteractions:
		return protocolrelaykit.GeminiInteractions
	default:
		return ""
	}
}

// providerTargetFor maps an adapter's type (its API surface) onto the wire
// format its upstream speaks. A client using another dialect is converted into
// it; the same dialect passes through untouched.
func providerTargetFor(source protocolrelaykit.Format, provider ProviderDialect) (protocolrelaykit.Format, bool) {
	switch provider.Type {
	case config.ProviderTypeOpenAI:
		return protocolrelaykit.OpenAIChat, source != protocolrelaykit.OpenAIChat
	case config.ProviderTypeOpenAIResponses:
		return protocolrelaykit.OpenAIResponses, source != protocolrelaykit.OpenAIResponses
	case config.ProviderTypeAnthropic:
		return protocolrelaykit.ClaudeMessages, source != protocolrelaykit.ClaudeMessages
	case config.ProviderTypeGemini:
		return protocolrelaykit.Gemini, source != protocolrelaykit.Gemini
	case config.ProviderTypeGeminiInteractions:
		return protocolrelaykit.GeminiInteractions, source != protocolrelaykit.GeminiInteractions
	default:
		return "", false
	}
}

func protocolTargetFor(source protocolrelaykit.Format, _ string) (protocolrelaykit.Format, bool) {
	switch source {
	case protocolrelaykit.OpenAIChat, protocolrelaykit.OpenAIResponses, protocolrelaykit.ClaudeMessages,
		protocolrelaykit.Gemini, protocolrelaykit.GeminiInteractions:
		// Local backends speak Chat Completions; every other dialect is
		// converted into it.
		return protocolrelaykit.OpenAIChat, source != protocolrelaykit.OpenAIChat
	default:
		return "", false
	}
}

func convertProtocolRequest(ctx context.Context, meta *protocolrelaykit.Meta, source, target protocolrelaykit.Format, body []byte, model string, st *store.Store) ([]byte, error) {
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
		return nil, fmt.Errorf("the request body is not valid JSON: %s", jsonErrorDetail(body, err))
	}
	delete(request, "previous_response_id")
	return json.Marshal(request)
}

// jsonErrorDetail describes a JSON syntax failure with the byte offset and a
// short preview of the bytes around it. A client that builds its body by hand
// (instead of escaping it) otherwise only learns that "decoding failed"; the
// offset and preview point at the exact content that broke it. %q renders the
// preview: quotes, backslashes and control bytes stay escaped, so the message
// is safe to show and to log.
func jsonErrorDetail(body []byte, err error) string {
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || syntax.Offset <= 0 || int(syntax.Offset) > len(body) {
		return err.Error()
	}
	offset := int(syntax.Offset)
	start := offset - 32
	if start < 0 {
		start = 0
	}
	end := offset + 32
	if end > len(body) {
		end = len(body)
	}
	return fmt.Sprintf("%s (byte %d of %d, near %q)", err.Error(), offset, len(body), string(body[start:end]))
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

// errProtocolBodyTooLarge reports an upstream response that cannot be
// buffered for conversion. The caller answers 502 instead of handing the
// client a body in the wrong dialect.
var errProtocolBodyTooLarge = errors.New("protocol response body exceeds the conversion limit")

// protocolResponseWriter delivers an upstream response written in the
// `upstream` dialect to a client speaking the `client` dialect. Streaming
// responses convert event by event; whole JSON responses are buffered and
// converted by finish.
type protocolResponseWriter struct {
	writer       http.ResponseWriter
	request      *http.Request
	store        *store.Store
	model        string
	backend      string
	upstream     protocolrelaykit.Format
	client       protocolrelaykit.Format
	requestBody  []byte
	meta         *protocolrelaykit.Meta
	stream       bool
	overflow     bool
	streamWriter *protocolStreamWriter
	buffer       *protocolBufferedWriter
}

// protocolBufferedWriter collects a whole JSON response so it can be converted.
type protocolBufferedWriter struct {
	parent    http.ResponseWriter
	header    http.Header
	body      bytes.Buffer
	status    int
	committed bool
}

// newProtocolResponseWriter builds the response writer for one converted
// exchange: a streaming converter for SSE, a buffering converter for whole
// JSON responses.
func newProtocolResponseWriter(w http.ResponseWriter, r *http.Request, st *store.Store, model, backend string, upstream, client protocolrelaykit.Format, requestBody []byte, meta *protocolrelaykit.Meta, stream bool) (*protocolResponseWriter, error) {
	writer := &protocolResponseWriter{
		writer: w, request: r, store: st, model: model, backend: backend,
		upstream: upstream, client: client, requestBody: requestBody, meta: meta, stream: stream,
	}
	if stream {
		streamWriter, err := newProtocolStreamWriter(w, r, st, model, backend, upstream, client, requestBody, meta)
		if err != nil {
			return nil, err
		}
		writer.streamWriter = streamWriter
		return writer, nil
	}
	writer.buffer = &protocolBufferedWriter{parent: w, header: w.Header().Clone()}
	return writer, nil
}

func (p *protocolResponseWriter) Header() http.Header {
	if p.streamWriter != nil {
		return p.streamWriter.Header()
	}
	return p.buffer.header
}

func (p *protocolResponseWriter) WriteHeader(status int) {
	if p.streamWriter != nil {
		p.streamWriter.WriteHeader(status)
		return
	}
	if p.buffer.status == 0 {
		p.buffer.status = status
	}
}

func (p *protocolResponseWriter) Write(data []byte) (int, error) {
	if p.streamWriter != nil {
		return p.streamWriter.Write(data)
	}
	if p.buffer.status == 0 {
		p.buffer.status = http.StatusOK
	}
	if p.overflow || p.buffer.body.Len()+len(data) > backendTransformBodyLimit {
		// Keep draining the upstream (the caller reports the limit) instead of
		// buffering a body we will never convert.
		p.overflow = true
		return len(data), nil
	}
	return p.buffer.body.Write(data)
}

// committed reports whether the response (or its head) already reached the
// client, which makes a later error message impossible to deliver.
func (p *protocolResponseWriter) committed() bool {
	if p.streamWriter != nil {
		return p.streamWriter.committed
	}
	return p.buffer.committed
}

func (p *protocolResponseWriter) Flush() {
	if p.streamWriter != nil {
		p.streamWriter.Flush()
	}
}

// finish converts the buffered response and commits it to the client.
func (p *protocolResponseWriter) finish() error {
	if p.streamWriter != nil {
		return p.streamWriter.finish()
	}
	if p.buffer.committed {
		return nil
	}
	if p.overflow {
		return fmt.Errorf("%w (%d bytes)", errProtocolBodyTooLarge, backendTransformBodyLimit)
	}
	status := p.buffer.status
	if status == 0 {
		status = http.StatusOK
	}
	body := append([]byte(nil), p.buffer.body.Bytes()...)
	transformed := false
	if status >= 200 && status < 300 {
		if len(bytes.TrimSpace(body)) != 0 {
			// A Responses object can carry status:"failed" with HTTP 200; the
			// converted body is then an error envelope and 200 would make it
			// look like an empty success.
			if protocolrelaykit.ResponseFailed(p.upstream, body) {
				status = http.StatusBadGateway
			}
			result, err := protocolrelaykit.ConvertResponse(p.request.Context(), p.meta, p.upstream, p.client, body)
			if err != nil {
				return err
			}
			logResponseConversion(result, p.model)
			body, err = protocolrelaykit.Marshal(result.Value)
			if err != nil {
				return err
			}
			transformed = true
		}
	} else {
		body = normalizeProtocolError(body, p.client, status)
		transformed = true
	}
	if transformed {
		p.buffer.header.Set("Content-Type", "application/json")
		clearTransformedBodyHeaders(p.buffer.header)
	}
	err := p.commit(status, body)
	if p.client == protocolrelaykit.OpenAIResponses && status >= 200 && status < 300 {
		persistResponseAffinity(p.request, p.store, p.model, p.backend, p.requestBody, body, status)
	}
	return err
}

// commit writes the converted response to the real client.
func (p *protocolResponseWriter) commit(status int, body []byte) error {
	if p.buffer.committed {
		return nil
	}
	copyHeaders(p.buffer.parent.Header(), p.buffer.header)
	p.buffer.parent.WriteHeader(status)
	p.buffer.committed = true
	if len(body) > 0 {
		_, err := p.buffer.parent.Write(body)
		return err
	}
	return nil
}

func logRequestConversion(result *protocolrelaykit.RequestResult, model string) {
	if result == nil {
		return
	}
	slog.Debug("protocol request converted", "model", model, "from", result.From, "to", result.To, "converter", result.Converter, "quality", result.Quality)
	if len(result.Diagnostics) > 0 {
		slog.Warn("protocol request conversion diagnostics", "model", model, "diagnostics", result.Diagnostics)
	}
}

func logResponseConversion(result *protocolrelaykit.ResponseResult, model string) {
	if result == nil {
		return
	}
	slog.Debug("protocol response converted", "model", model, "from", result.From, "to", result.To, "converter", result.Converter, "quality", result.Quality)
	if len(result.Diagnostics) > 0 {
		slog.Warn("protocol response conversion diagnostics", "model", model, "diagnostics", result.Diagnostics)
	}
}

func normalizeProtocolError(body []byte, target protocolrelaykit.Format, status int) []byte {
	return protocolrelaykit.ErrorEnvelope(body, target, status)
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
	meta          *protocolrelaykit.Meta
	state         *protocolrelaykit.ResponseStreamState
	streamOptions protocolrelaykit.StreamOptions
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

func newProtocolStreamWriter(w http.ResponseWriter, r *http.Request, st *store.Store, model, backend string, upstream, client protocolrelaykit.Format, requestBody []byte, meta *protocolrelaykit.Meta) (*protocolStreamWriter, error) {
	options := protocolrelaykit.StreamOptions{
		Model: model, Created: time.Now().Unix(), IncludeUsage: true,
		EstimateInputTokens: meta.EstimatePromptTokens,
	}
	var state *protocolrelaykit.ResponseStreamState
	if upstream != protocolrelaykit.OpenAIChat || client != protocolrelaykit.OpenAIResponses {
		var err error
		state, err = protocolrelaykit.NewResponseStreamState(upstream, client, options)
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
	// Pick the earliest terminator. Searching "\n\n" first used to merge two
	// events when an upstream mixed LF and CRLF delimiters, because the CRLF
	// terminator contains no "\n\n" but the following LF event does.
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	index, separatorLength := -1, 0
	switch {
	case lf >= 0 && (crlf < 0 || lf < crlf):
		index, separatorLength = lf, 2
	case crlf >= 0:
		index, separatorLength = crlf, 4
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
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		// The SSE grammar strips exactly one leading space after the colon;
		// trimming all whitespace corrupted payloads whose own content starts
		// or ends with a significant space.
		payload := strings.TrimPrefix(line, "data:")
		payload = strings.TrimPrefix(payload, " ")
		lines = append(lines, payload)
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
		w.state, err = protocolrelaykit.NewResponseStreamState(w.upstream, w.client, w.streamOptions)
		if err != nil {
			return err
		}
	}
	results, err := protocolrelaykit.ConvertStreamResponseChunk(w.request.Context(), w.meta, w.state, response)
	if err != nil {
		return err
	}
	return w.writeResults(results)
}

func (w *protocolStreamWriter) writeResults(results []protocolrelaykit.ResponseResult) error {
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

// forwardBareJSON converts one whole non-streaming upstream body into the
// client dialect and writes it as a single frame. It is the fallback for an
// upstream that promised SSE and sent JSON.
func (w *protocolStreamWriter) forwardBareJSON(body []byte) error {
	converted, err := protocolrelaykit.ConvertResponse(w.request.Context(), w.meta, w.upstream, w.client, body)
	if err != nil {
		return err
	}
	logResponseConversion(converted, w.model)
	encoded, err := protocolrelaykit.Marshal(converted.Value)
	if err != nil {
		return err
	}
	if w.client == protocolrelaykit.OpenAIResponses {
		// A bare body has no event envelope; wrap it so the client sees a
		// completed response rather than a dangling stream.
		payload, marshalErr := json.Marshal(map[string]any{
			"type": "response.completed", "response": json.RawMessage(encoded),
		})
		if marshalErr == nil {
			encoded = payload
			if _, writeErr := w.writer.Write([]byte("event: response.completed\ndata: " + string(encoded) + "\n\n")); writeErr != nil {
				return writeErr
			}
			return nil
		}
	}
	if _, err := w.writer.Write([]byte("data: " + string(encoded) + "\n\n")); err != nil {
		return err
	}
	return nil
}

// emitTerminalStreamError writes a final frame the client dialect can parse as
// a failure. It is best-effort: the response is already committed.
func (w *protocolStreamWriter) emitTerminalStreamError(cause error) {
	message := cause.Error()
	var frames [][]byte
	switch w.client {
	case protocolrelaykit.ClaudeMessages:
		payload, err := json.Marshal(map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": message},
		})
		if err == nil {
			frames = append(frames, []byte("event: error\ndata: "+string(payload)+"\n\n"))
		}
	case protocolrelaykit.Gemini:
		payload, err := json.Marshal(map[string]any{
			"error": map[string]any{"code": http.StatusBadGateway, "message": message, "status": "INTERNAL"},
		})
		if err == nil {
			frames = append(frames, []byte("data: "+string(payload)+"\n\n"))
		}
	case protocolrelaykit.OpenAIResponses:
		errorObject := map[string]any{"code": "stream_conversion_failed", "message": message}
		if payload, err := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": w.streamOptions.ID, "object": "response", "status": "failed",
				"model": w.model, "output": []any{}, "error": errorObject,
			},
		}); err == nil {
			frames = append(frames, []byte("event: response.failed\ndata: "+string(payload)+"\n\n"))
		}
	default:
		payload, err := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "stream_conversion_error"},
		})
		if err == nil {
			frames = append(frames, []byte("data: "+string(payload)+"\n\n"))
		}
		if done := protocolrelaykit.DoneSSE(w.client); len(done) > 0 {
			frames = append(frames, done)
		}
	}
	for _, frame := range frames {
		if _, err := w.writer.Write(frame); err != nil {
			return
		}
	}
	w.flush()
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
		// The head is already committed, so the error cannot become a status
		// code. Emit a terminal error frame in the client dialect instead of
		// ending the stream silently: a bare EOF looks like a truncated but
		// successful answer to most SDKs.
		w.emitTerminalStreamError(w.streamErr)
		return w.streamErr
	}
	if w.pending.Len() > 0 {
		pending := append([]byte(nil), w.pending.Bytes()...)
		w.pending.Reset()
		// An upstream can ignore alt=sse and answer with one bare JSON body.
		// Treating it as a malformed SSE event used to emit the opening frames
		// and then nothing, so the client saw an empty successful answer.
		if !bytes.Contains(pending, []byte("data:")) && json.Valid(pending) {
			if err := w.forwardBareJSON(pending); err != nil {
				return err
			}
		} else if err := w.convertEvent(pending); err != nil {
			return err
		}
	}
	if w.state == nil {
		state, err := protocolrelaykit.NewResponseStreamState(w.upstream, w.client, w.streamOptions)
		if err != nil {
			return err
		}
		w.state = state
	}
	results, err := protocolrelaykit.FinalizeStreamResponse(w.request.Context(), w.meta, w.state)
	if err != nil {
		return err
	}
	if err := w.writeResults(results); err != nil {
		return err
	}
	// The Chat encoder emits [DONE] itself as its terminal frame; writing a
	// second sentinel here gave every converted stream two of them.

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
