package swaputil

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/route"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConstantTimeEquals compares two API key strings in constant time. Hashing
// both sides first keeps the comparison length-invariant, so the digest
// length never leaks the secret's length or match position.
func ConstantTimeEquals(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return hmac.Equal(ha[:], hb[:])
}

type contextkey struct {
	name string
}

type ReqContextData struct {
	ApiKey           string
	Model            string
	ModelID          string
	Streaming        bool
	SendLoadingState bool
	// Metadata is a request-scoped key/value bag that handlers may mutate
	// while processing. The metrics middleware copies it into ActivityLogEntry.
	Metadata map[string]string
}

const (
	MaxMultiPartSize   = 32 << 20
	MaxRequestBodySize = 64 << 20
)

var (
	ReqContextKey        = &contextkey{"context"}
	ErrNoModelInContext  = fmt.Errorf("no model in request context")
	ErrNoRouterFound     = fmt.Errorf("no router found for model")
	ErrNoPeerModelFound  = fmt.Errorf("peer model not found")
	ErrNoLocalModelFound = fmt.Errorf("local model not found")
	ErrAmbiguousModel    = fmt.Errorf("model is ambiguous in request context")
	// ErrAmbiguousModelInContext is kept as an explicit alias for callers that
	// distinguish an omitted-model routing failure from other ambiguous model
	// errors. Both names unwrap to the same sentinel and therefore preserve the
	// existing HTTP 400 mapping in SendError.
	ErrAmbiguousModelInContext = ErrAmbiguousModel
	ErrRequestBodyTooLarge     = fmt.Errorf("request body is too large")
)

var modelRouteRegistry = func() *route.Registry {
	registry, err := route.NewRegistry(route.DefaultDescriptors())
	if err != nil {
		// DefaultDescriptors is compiled into the binary. Keep request parsing
		// fail-closed if a future edit makes the registry invalid.
		return nil
	}
	return registry
}()

// IsWebSocketUpgrade reports whether r contains a valid websocket protocol
// upgrade request. Header token comparisons are case-insensitive and support
// comma-separated or repeated header values.
func IsWebSocketUpgrade(r *http.Request) bool {
	return headerContainsToken(r.Header.Values("Connection"), "upgrade") &&
		headerContainsToken(r.Header.Values("Upgrade"), "websocket")
}

func headerContainsToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ShouldIgnoreWebsocket reports whether r is a websocket request whose local
// model configuration opts out of websocket lifecycle activity.
func ShouldIgnoreWebsocket(r *http.Request, cfg config.Config) bool {
	if !IsWebSocketUpgrade(r) {
		return false
	}
	data, err := FetchContext(r, cfg)
	if err != nil {
		return false
	}
	mc, ok := cfg.Models[data.ModelID]
	return ok && mc.Compat.IgnoreWebsockets
}

// StatusClientClosedRequest mirrors nginx's non-standard 499 "Client Closed
// Request". llama-swap records it when a client disconnects before any
// response was written, so an abandoned request is not filed as a success (a
// cancelled cold-start load) or blamed on the upstream (a 502 from the reverse
// proxy). It is never written to the connection — the client is already gone —
// so it only ever appears in the access log and the activity store.
const StatusClientClosedRequest = 499

// StatusMarker is implemented by the response recorders in the middleware
// chain. It lets a status be recorded for logging and metrics without writing
// anything to the client.
type StatusMarker interface {
	// MarkStatus records code as the response status without writing it to
	// the connection. Recorders that wrap another writer forward the call so
	// every recorder in the chain agrees on the status.
	MarkStatus(code int)
	// WroteHeader reports whether a response status has already been written.
	WroteHeader() bool
}

// clientCtxKey carries the connection's own context past middleware that derive
// cancellable child contexts from it.
type clientCtxKey struct{}

// SetClientContext returns ctx carrying clientCtx as the client connection's
// context, so a later cancellation can be attributed correctly.
func SetClientContext(ctx, clientCtx context.Context) context.Context {
	return context.WithValue(ctx, clientCtxKey{}, clientCtx)
}

// WithClientContext returns r with its current context remembered as the client
// connection's own. Call it before any middleware derives a cancellable child
// context, so a request aborted server-side is not mistaken for a client that
// hung up.
func WithClientContext(r *http.Request) *http.Request {
	return r.WithContext(SetClientContext(r.Context(), r.Context()))
}

// ClientContext returns the client connection's context as remembered by
// WithClientContext, falling back to ctx itself when none was recorded.
func ClientContext(ctx context.Context) context.Context {
	if clientCtx, ok := ctx.Value(clientCtxKey{}).(context.Context); ok {
		return clientCtx
	}
	return ctx
}

// ResponseStarted reports whether a response status has already reached the
// client, in which case an error handler has nothing useful left to send.
func ResponseStarted(w http.ResponseWriter) bool {
	marker, ok := w.(StatusMarker)
	return ok && marker.WroteHeader()
}

// MarkClientClosed records StatusClientClosedRequest on w when the client
// connection itself went away before any response status was written. It
// reports whether the sentinel was recorded.
//
// The test is deliberately the client's own context rather than the request's:
// middleware derive cancellable children from it (the inflight tracker, which
// an operator can cancel from the UI, and the peer router's shutdown link), and
// a request killed server-side still has a live client that must be answered
// normally. Blaming that client would be the same misattribution this sentinel
// exists to fix.
//
// When it does apply, nothing is sent: the connection is already gone, and on a
// streamed response the upstream may have flushed headers long ago, where a
// late WriteHeader would only produce "superfluous response.WriteHeader" spam.
// A response that already started keeps the status it really had.
func MarkClientClosed(w http.ResponseWriter, r *http.Request) bool {
	marker, ok := w.(StatusMarker)
	if !ok || marker.WroteHeader() || ClientContext(r.Context()).Err() == nil {
		return false
	}
	marker.MarkStatus(StatusClientClosedRequest)
	return true
}

func SendError(w http.ResponseWriter, r *http.Request, err error) {
	// Guarded here as well as in SendResponse because the HTTPError branch
	// below writes its own body rather than delegating. See SendResponse.
	if ResponseStarted(w) {
		return
	}

	var httpErr HTTPError
	if errors.As(err, &httpErr) {
		for k, v := range httpErr.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(httpErr.StatusCode())
		w.Write(httpErr.Body())
		return
	}

	switch {
	case errors.Is(err, ErrNoModelInContext):
		SendResponse(w, r, http.StatusNotFound, "no model id could be identified")
	case errors.Is(err, ErrNoPeerModelFound):
		SendResponse(w, r, http.StatusNotFound, "no peer found for requested model")
	case errors.Is(err, ErrNoLocalModelFound):
		SendResponse(w, r, http.StatusNotFound, "no local server found for requested model")
	case errors.Is(err, ErrAmbiguousModel):
		SendResponse(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrRequestBodyTooLarge):
		SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, ErrNoRouterFound):
		SendResponse(w, r, http.StatusNotFound, "no router for requested model")
	default:
		SendResponse(w, r, http.StatusInternalServerError, fmt.Sprintf("unspecific error: %v", err))
	}
}

// SendResponse detects what content type the client prefers and returns an
// error response in that format. JSON responses use the OpenAI-compatible
// envelope, where "error" is an object rather than a string.
func SendResponse(w http.ResponseWriter, r *http.Request, status int, message string) {
	// A response that already started cannot carry a status any more, and this
	// body would be appended to whatever the client is mid-way through reading
	// — corrupting a stream rather than reporting the error. Callers that can
	// still report in-band (the loading stream frames it as SSE) do so before
	// reaching here; for the rest, dropping it is the lesser harm.
	if ResponseStarted(w) {
		return
	}

	acceptHeader := r.Header.Get("Accept")
	if strings.Contains(acceptHeader, "text/plain") {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		w.Write([]byte(fmt.Sprintf("llama-swap: %s", message)))
		return
	}

	if strings.Contains(acceptHeader, "text/html") {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		w.Write([]byte(fmt.Sprintf(`<html><body><h1>llama-swap</h1><p>%s</p></body></html>`, html.EscapeString(message))))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(NewErrorEnvelope(status, message, "").JSON())
}

// FetchContext will attempt to get the model id from the context, then
// from an /upstream/<model> path prefix, then from the request body/query.
// If it extracts the model it will store it in the context for downstream
// handlers. An error will be returned when a model cannot be identified.
func FetchContext(r *http.Request, cfg config.Config) (ReqContextData, error) {
	data, ok := ReadContext(r.Context())
	if ok {
		return data, nil
	}

	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		if data, ok := extractUpstreamContext(r, cfg); ok {
			*r = *r.WithContext(SetContext(r.Context(), data))
			return data, nil
		}
		return ReqContextData{}, ErrNoModelInContext
	}

	data, extractErr := extractContext(r)
	if errors.Is(extractErr, ErrRequestBodyTooLarge) {
		return ReqContextData{}, extractErr
	}
	if extractErr == nil && data.Model == "" && modelOptionalForRequest(r) {
		model, inferErr := inferOptionalModel(cfg)
		if inferErr != nil {
			return ReqContextData{}, inferErr
		}
		data.Model = model
	}
	if extractErr == nil && data.Model != "" {
		realName, _ := cfg.RealModelName(data.Model)
		if realName == "" {
			realName = data.Model
		}
		data.ModelID = realName
		if mc, ok := cfg.Models[realName]; ok {
			data.SendLoadingState = mc.SendLoadingState != nil && *mc.SendLoadingState
		}
		*r = *r.WithContext(SetContext(r.Context(), data))
		return data, nil
	}

	return ReqContextData{}, ErrNoModelInContext
}

// modelOptionalForRequest consults the route registry rather than duplicating
// path checks in request parsing and authorization. It intentionally matches
// the original public path; version-prefix normalization happens only after
// the request context and authorization middleware have run.
func modelOptionalForRequest(r *http.Request) bool {
	if r == nil || modelRouteRegistry == nil {
		return false
	}
	descriptor, _, ok := modelRouteRegistry.Match(r.Method, r.URL.Path)
	return ok && descriptor.ModelOptional
}

// ModelOptionalForRequest reports whether the request's protocol descriptor
// permits omitting a model selector. It is used by auth middleware before the
// normal request-context middleware runs.
func ModelOptionalForRequest(r *http.Request) bool {
	return modelOptionalForRequest(r)
}

// inferOptionalModel resolves an omitted model only when exactly one local
// model is configured. A request cannot safely be dispatched to an arbitrary
// model when multiple local backends exist, so that case is an explicit 400.
// Peer models and selectors are deliberately excluded: neither can be chosen
// without an unambiguous public model ID.
func inferOptionalModel(cfg config.Config) (string, error) {
	models := make([]string, 0, len(cfg.Models))
	for modelID := range cfg.Models {
		if strings.TrimSpace(modelID) != "" {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	switch len(models) {
	case 0:
		return "", ErrNoModelInContext
	case 1:
		return models[0], nil
	default:
		return "", fmt.Errorf("%w: specify model explicitly; configured local models are %s", ErrAmbiguousModel, strings.Join(models, ", "))
	}
}

// ExtractModel returns the model name encoded in a request without caching
// request data in its context.
func ExtractModel(r *http.Request) (string, error) {
	data, err := extractContext(r)
	return data.Model, err
}

// ReplaceRequestModel replaces model with replacement wherever the request
// encodes its model ID. It returns a request whose cached model context has
// been invalidated so downstream handlers resolve the replacement normally.
func ReplaceRequestModel(r *http.Request, model, replacement string) (*http.Request, error) {
	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		upstreamPath := strings.TrimPrefix(r.PathValue("upstreamPath"), "/")
		if upstreamPath != model && !strings.HasPrefix(upstreamPath, model+"/") {
			return r, nil
		}

		// Preserve the client's escaping of the path after the model name before
		// URL.Path is rewritten below.
		escapedRemaining := EscapedPathSuffix(r.URL.EscapedPath(), "/upstream/"+model)

		remainingPath := strings.TrimPrefix(upstreamPath, model)
		rewrittenPath := replacement + remainingPath
		if replacement == "" {
			rewrittenPath = ""
		}
		r.SetPathValue("upstreamPath", rewrittenPath)
		r.URL.Path = "/upstream/" + rewrittenPath
		r.URL.RawPath = ""
		if replacement != "" && escapedRemaining != "" {
			prefix := (&url.URL{Path: "/upstream/" + replacement}).EscapedPath()
			r.URL.RawPath = prefix + escapedRemaining
		}
		return invalidateRequestContext(r), nil
	}

	current, err := ExtractModel(r)
	if err != nil {
		return r, err
	}
	if current != model {
		return r, nil
	}

	if r.Method == http.MethodGet {
		query := r.URL.Query()
		query.Set("model", replacement)
		r.URL.RawQuery = query.Encode()
		return invalidateRequestContext(r), nil
	}

	// Same case-insensitivity as extractContext: an "Application/JSON" request
	// must still have its model replaced.
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "application/json"):
		if r.Body == nil || r.Body == http.NoBody {
			return r, fmt.Errorf("could not read request body")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodySize+1))
		if err != nil {
			return r, fmt.Errorf("could not read request body")
		}
		if len(body) > MaxRequestBodySize {
			_ = r.Body.Close()
			return r, fmt.Errorf("%w: request body exceeds %d bytes", ErrRequestBodyTooLarge, MaxRequestBodySize)
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		body, err = sjson.SetBytes(body, "model", replacement)
		if err != nil {
			return r, fmt.Errorf("could not rewrite model in JSON body: %w", err)
		}
		replaceRequestBody(r, body)
	case strings.Contains(contentType, "multipart/form-data"):
		if err := bufferRequestBodyForRewrite(r); err != nil {
			return r, err
		}
		if err := r.ParseMultipartForm(MaxMultiPartSize); err != nil {
			return r, fmt.Errorf("could not parse multipart form: %w", err)
		}
		form := r.MultipartForm
		defer form.RemoveAll()
		body, rewrittenContentType, err := replaceMultipartModel(form, replacement)
		if err != nil {
			return r, err
		}
		r.MultipartForm = nil
		r.Form = nil
		r.PostForm = nil
		r.Header.Set("Content-Type", rewrittenContentType)
		replaceRequestBody(r, body)
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		if err := bufferRequestBodyForRewrite(r); err != nil {
			return r, err
		}
		if err := r.ParseForm(); err != nil {
			return r, fmt.Errorf("could not parse form: %w", err)
		}
		r.PostForm.Set("model", replacement)
		replaceRequestBody(r, []byte(r.PostForm.Encode()))
	default:
		if err := bufferRequestBodyForRewrite(r); err != nil {
			return r, err
		}
		if err := r.ParseForm(); err != nil {
			return r, fmt.Errorf("could not parse form: %w", err)
		}
		r.PostForm.Set("model", replacement)
		replaceRequestBody(r, []byte(r.PostForm.Encode()))
	}

	return invalidateRequestContext(r), nil
}

func invalidateRequestContext(r *http.Request) *http.Request {
	if _, ok := ReadContext(r.Context()); !ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ReqContextKey, struct{}{}))
}

func replaceRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Del("Transfer-Encoding")
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.ContentLength = int64(len(body))
}

// bufferRequestBodyForRewrite gives form-based model rewrites the same hard
// request-size boundary as JSON extraction. The /upstream/<model>/... path
// intentionally derives the model from the URL and therefore bypasses the
// normal context parser; without this guard ParseMultipartForm/ParseForm
// could otherwise consume an unbounded body before the proxy forwards it.
func bufferRequestBodyForRewrite(r *http.Request) error {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodySize+1))
	_ = r.Body.Close()
	if err != nil {
		return fmt.Errorf("could not read request body: %w", err)
	}
	if len(body) > MaxRequestBodySize {
		return fmt.Errorf("%w: request body exceeds %d bytes", ErrRequestBodyTooLarge, MaxRequestBodySize)
	}
	replaceRequestBody(r, body)
	return nil
}

func replaceMultipartModel(form *multipart.Form, replacement string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for key, values := range form.Value {
		for _, value := range values {
			if key == "model" {
				value = replacement
			}
			field, err := mw.CreateFormField(key)
			if err != nil {
				return nil, "", fmt.Errorf("error recreating form field %s: %w", key, err)
			}
			if _, err := field.Write([]byte(value)); err != nil {
				return nil, "", fmt.Errorf("error writing form field %s: %w", key, err)
			}
		}
	}

	for key, headers := range form.File {
		for _, fh := range headers {
			part, err := mw.CreateFormFile(key, fh.Filename)
			if err != nil {
				return nil, "", fmt.Errorf("error recreating form file %s: %w", key, err)
			}
			file, err := fh.Open()
			if err != nil {
				return nil, "", fmt.Errorf("error opening uploaded file %s: %w", key, err)
			}
			if _, err := io.Copy(part, file); err != nil {
				file.Close()
				return nil, "", fmt.Errorf("error copying file data %s: %w", key, err)
			}
			file.Close()
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("error finalizing multipart form: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// extractUpstreamContext resolves the model from an /upstream/<model>/... path.
func extractUpstreamContext(r *http.Request, cfg config.Config) (ReqContextData, bool) {
	searchName, realName, _, found := FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
	if !found {
		return ReqContextData{}, false
	}
	return ReqContextData{
		Model:            searchName,
		ModelID:          realName,
		ApiKey:           ExtractAPIKey(r),
		Streaming:        r.URL.Query().Get("stream") == "true",
		SendLoadingState: sendLoadingState(cfg, realName),
		Metadata:         make(map[string]string),
	}, true
}

// sendLoadingState reports whether the configured model wants loading-state SSEs.
func sendLoadingState(cfg config.Config, modelID string) bool {
	if mc, ok := cfg.Models[modelID]; ok {
		return mc.SendLoadingState != nil && *mc.SendLoadingState
	}
	return false
}

// FindModelInPath walks a slash-separated path, building up segments until one
// matches a configured model. This resolves model names that contain slashes
// (e.g. "author/model"). Returns the matched name, its real model ID, the
// remaining path, and whether a match was found.
func FindModelInPath(cfg config.Config, path string) (searchName, realName, remainingPath string, found bool) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	name := ""

	for i, part := range parts {
		if part == "" {
			continue
		}
		if name == "" {
			name = part
		} else {
			name = name + "/" + part
		}

		if modelID, ok := cfg.ResolveBaseModel(name); ok {
			searchName = name
			realName = modelID
			remainingPath = "/" + strings.Join(parts[i+1:], "/")
			found = true
		}
	}

	return
}

// EscapedPathSuffix removes decodedPrefix from escapedPath while leaving the
// remaining percent-encoding untouched. Decoded paths cannot identify the
// boundary alone: a model name such as "author/model" may have arrived as the
// single escaped segment "author%2Fmodel".
func EscapedPathSuffix(escapedPath, decodedPrefix string) string {
	rawIndex, prefixIndex := 0, 0
	for rawIndex < len(escapedPath) && prefixIndex < len(decodedPrefix) {
		var end int
		if escapedPath[rawIndex] == '%' {
			end = rawIndex + 3
		} else {
			_, size := utf8.DecodeRuneInString(escapedPath[rawIndex:])
			end = rawIndex + size
		}
		if end > len(escapedPath) {
			return ""
		}

		decoded, err := url.PathUnescape(escapedPath[rawIndex:end])
		if err != nil || !strings.HasPrefix(decodedPrefix[prefixIndex:], decoded) {
			return ""
		}
		rawIndex = end
		prefixIndex += len(decoded)
	}
	if prefixIndex != len(decodedPrefix) {
		return ""
	}
	return escapedPath[rawIndex:]
}

func SetContext(ctx context.Context, data ReqContextData) context.Context {
	return context.WithValue(ctx, ReqContextKey, data)
}

func ReadContext(ctx context.Context) (ReqContextData, bool) {
	data, ok := ctx.Value(ReqContextKey).(ReqContextData)
	return data, ok
}

// SetReqData attaches a key/value pair to the request context's metadata map.
// The metadata map must already exist in the context's ReqContextData; callers
// should ensure FetchContext has run or initialize the map themselves.
// It returns an error for nil contexts or contexts without request data.
func SetReqData(ctx context.Context, key, value string) error {
	if ctx == nil {
		return fmt.Errorf("cannot set request metadata on nil context")
	}
	data, ok := ReadContext(ctx)
	if !ok {
		return fmt.Errorf("no request context data found")
	}
	if data.Metadata == nil {
		return fmt.Errorf("no metadata map in request context")
	}
	data.Metadata[key] = value
	return nil
}

// extractContext pulls fields from an HTTP request into a ReqContextData,
// returning whatever is available. For GET requests it reads query parameters.
// For POST requests it inspects Content-Type and parses JSON,
// multipart/form-data, or application/x-www-form-urlencoded bodies. The
// request body is always restored before returning. An error is returned only
// for I/O or parse failures, not for missing fields.
func extractContext(r *http.Request) (ReqContextData, error) {

	apiKey := ExtractAPIKey(r)

	if r.Method == http.MethodGet {
		q := r.URL.Query()
		return ReqContextData{
			Model:     q.Get("model"),
			Streaming: q.Get("stream") == "true",
			ApiKey:    apiKey,
			Metadata:  make(map[string]string),
		}, nil
	}

	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var readErr error
		bodyBytes, readErr = io.ReadAll(io.LimitReader(r.Body, MaxRequestBodySize+1))
		if readErr != nil {
			return ReqContextData{}, fmt.Errorf("error reading request body: %w", readErr)
		}
		if len(bodyBytes) > MaxRequestBodySize {
			defer func() {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}()
			return ReqContextData{}, fmt.Errorf("%w: request body exceeds %d bytes", ErrRequestBodyTooLarge, MaxRequestBodySize)
		}
	}
	defer func() {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}()

	// Media types are case-insensitive (RFC 9110), so "Application/JSON" is a
	// valid JSON request. Matching case-sensitively sent it down the form parser
	// instead, where no model could be found and the client got a 404.
	contentType := strings.ToLower(r.Header.Get("Content-Type"))

	if strings.Contains(contentType, "application/json") {
		return ReqContextData{
			Model:     gjson.GetBytes(bodyBytes, "model").String(),
			Streaming: gjson.GetBytes(bodyBytes, "stream").Bool(),
			ApiKey:    apiKey,
			Metadata:  make(map[string]string),
		}, nil
	}

	// Form parsers read from r.Body, so feed them a fresh reader over the
	// buffered bytes. The deferred restore above will reset r.Body again
	// after parsing.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(MaxMultiPartSize); err != nil {
			return ReqContextData{}, fmt.Errorf("error parsing multipart form: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return ReqContextData{}, fmt.Errorf("error parsing form: %w", err)
		}
	}

	return ReqContextData{
		Model:     r.FormValue("model"),
		Streaming: r.FormValue("stream") == "true",
		ApiKey:    apiKey,
		Metadata:  make(map[string]string),
	}, nil
}

// extractAPIKey pulls a candidate API key from the request, preferring Basic,
// then Bearer, then x-api-key.
func ExtractAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	var bearerKey, basicKey string
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, credentials, ok := splitAuthHeader(auth)
		if ok {
			switch strings.ToLower(scheme) {
			case "bearer":
				bearerKey = credentials
			case "basic":
				if decoded, err := base64.StdEncoding.DecodeString(credentials); err == nil {
					if parts := strings.SplitN(string(decoded), ":", 2); len(parts) == 2 {
						basicKey = parts[1] // password field is the API key
					}
				}
			}
		}
	}

	switch {
	case basicKey != "":
		return basicKey
	case bearerKey != "":
		return bearerKey
	case strings.TrimSpace(r.Header.Get("x-api-key")) != "":
		return strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	// Browsers cannot attach a custom Authorization header to EventSource.
	// The control plane therefore accepts the short-lived, same-origin session
	// cookie created by /api/auth/session as a final fallback. URL escaping keeps
	// legacy keys containing cookie-special characters round-trippable.
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if value, err := url.QueryUnescape(cookie.Value); err == nil {
			return value
		}
	}
	return ""
}

// splitAuthHeader separates an HTTP authentication scheme from its opaque
// credentials while accepting optional whitespace (including HTAB) between
// them. Credentials themselves are trimmed because API key formats do not
// contain whitespace; this keeps proxy-added padding from changing the key
// value without accepting a header that contains multiple credentials.
func splitAuthHeader(value string) (scheme, credentials string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false
	}
	separator := strings.IndexFunc(value, unicode.IsSpace)
	if separator < 0 {
		return "", "", false
	}
	scheme = value[:separator]
	credentials = strings.TrimSpace(value[separator:])
	if scheme == "" || credentials == "" {
		return "", "", false
	}
	for _, r := range credentials {
		if unicode.IsSpace(r) {
			return "", "", false
		}
	}
	return scheme, credentials, true
}
