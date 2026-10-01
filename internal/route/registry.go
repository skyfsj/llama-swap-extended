// Package route contains the declarative public API surface used by backend
// adapters. Keeping endpoint ownership here prevents server.go from growing a
// new path-specific special case for every backend.
package route

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type BodyKind string

// MethodWebSocket is a pseudo-method used by the route registry for
// WebSocket upgrade handlers. HTTP itself has no WebSocket method.
const MethodWebSocket = "WEBSOCKET"

const (
	BodyJSON BodyKind = "json"
	BodyForm BodyKind = "form"
	// BodyBinary describes an opaque request payload (for example an audio or
	// embedding-specific binary upload) whose model selector is carried by a
	// query/header/path rather than decoded as JSON. Keeping it in the registry
	// lets adapters declare the wire shape without adding another path-specific
	// special case in server.go.
	BodyBinary    BodyKind = "binary"
	BodyQuery     BodyKind = "query"
	BodyWebSocket BodyKind = "websocket"
)

type AdapterKind string

const (
	AdapterNative          AdapterKind = "native"
	AdapterResponsesToChat AdapterKind = "responsesToChat"
)

type RouteDescriptor struct {
	Method        string   `json:"method"`
	Pattern       string   `json:"pattern"`
	APIFamily     string   `json:"apiFamily,omitempty"`
	Body          BodyKind `json:"body"`
	ModelField    string   `json:"modelField,omitempty"`
	ModelLocation string   `json:"modelLocation,omitempty"` // body|query|path|header|frame
	// ModelOptional marks routes where the upstream protocol permits omitting
	// the model selector. The gateway may still need to infer one to choose a
	// configured backend; inference is only safe when the configured model set
	// is unambiguous.
	ModelOptional bool        `json:"modelOptional,omitempty"`
	Streaming     string      `json:"streaming,omitempty"` // normal|sse|websocket
	Capability    string      `json:"capability,omitempty"`
	Adapter       AdapterKind `json:"adapter,omitempty"`
	NeedsAffinity bool        `json:"needsAffinity,omitempty"`
	Permission    string      `json:"permission"`
	Management    bool        `json:"management,omitempty"`
}

func (d RouteDescriptor) Validate() error {
	if d.Method == "" || d.Pattern == "" {
		return fmt.Errorf("route method and pattern are required")
	}
	if !strings.HasPrefix(d.Pattern, "/") || strings.ContainsAny(d.Pattern, "?#\x00") {
		return fmt.Errorf("route %s %s: pattern must be an absolute path without query or fragment", d.Method, d.Pattern)
	}
	if err := validatePattern(d.Pattern); err != nil {
		return fmt.Errorf("route %s %s: %w", d.Method, d.Pattern, err)
	}
	if _, ok := map[string]bool{
		http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
		http.MethodPatch: true, http.MethodDelete: true,
		http.MethodOptions: true, MethodWebSocket: true,
	}[strings.ToUpper(d.Method)]; !ok {
		return fmt.Errorf("route %s %s: unsupported method", d.Method, d.Pattern)
	}
	switch d.Body {
	case "", BodyJSON, BodyForm, BodyBinary, BodyQuery, BodyWebSocket:
	default:
		return fmt.Errorf("route %s %s: unsupported body kind %q", d.Method, d.Pattern, d.Body)
	}
	switch d.Adapter {
	case "", AdapterNative, AdapterResponsesToChat:
	default:
		return fmt.Errorf("route %s %s: unsupported adapter %q", d.Method, d.Pattern, d.Adapter)
	}
	if d.ModelLocation != "" {
		switch d.ModelLocation {
		case "body", "query", "path", "header", "form", "frame":
		default:
			return fmt.Errorf("route %s %s: unsupported model location %q", d.Method, d.Pattern, d.ModelLocation)
		}
	}
	if d.ModelField != "" && d.ModelLocation == "" {
		return fmt.Errorf("route %s %s: model field requires a model location", d.Method, d.Pattern)
	}
	if d.ModelOptional && d.ModelField == "" {
		return fmt.Errorf("route %s %s: optional model requires a model field", d.Method, d.Pattern)
	}
	if strings.EqualFold(d.Method, MethodWebSocket) && d.Body != BodyWebSocket {
		return fmt.Errorf("route %s %s: websocket method requires websocket body", d.Method, d.Pattern)
	}
	if d.Streaming != "" {
		switch d.Streaming {
		case "normal", "sse", "websocket":
		default:
			return fmt.Errorf("route %s %s: unsupported streaming kind %q", d.Method, d.Pattern, d.Streaming)
		}
	}
	if d.Management && d.Permission == "" {
		return fmt.Errorf("route %s %s: management route requires permission", d.Method, d.Pattern)
	}
	return nil
}

func validatePattern(pattern string) error {
	parts := routePathParts(pattern)
	seen := make(map[string]struct{})
	for index, part := range parts {
		if !strings.ContainsAny(part, "{}") {
			continue
		}
		if !strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") {
			return fmt.Errorf("malformed path placeholder %q", part)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
		catchAll := strings.HasSuffix(name, "...")
		if catchAll {
			name = strings.TrimSuffix(name, "...")
			if index != len(parts)-1 {
				return fmt.Errorf("catch-all placeholder must be the final path segment")
			}
		}
		if !validPlaceholderName(name) {
			return fmt.Errorf("invalid path placeholder %q", name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate path placeholder %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validPlaceholderName(name string) bool {
	if name == "" {
		return false
	}
	for index, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

// DefaultDescriptors is the single source of truth for the standard model
// API families. Legacy llama.cpp-only paths are included in the server's
// compatibility registry separately.
func DefaultDescriptors() []RouteDescriptor {
	const inference = "inference"
	return []RouteDescriptor{
		{Method: http.MethodPost, Pattern: "/v1/completions", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "completions", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/chat/completions", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "chat", Streaming: "sse", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/chat/completions/batch", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", ModelOptional: true, Capability: "chat", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/batches", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "chat", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/responses", APIFamily: "openai.responses", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "responses", Adapter: AdapterNative, NeedsAffinity: true, Streaming: "sse", Permission: inference},
		{Method: http.MethodGet, Pattern: "/v1/responses/{response_id}", APIFamily: "openai.responses", Body: BodyQuery, NeedsAffinity: true, Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/responses/{response_id}/cancel", APIFamily: "openai.responses", Body: BodyQuery, NeedsAffinity: true, Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/embeddings", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "embeddings", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/classify", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "classify", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/pooling", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "pooling", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/generative_scoring", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "generative_scoring", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/embed", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "embed", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/audio/transcriptions", APIFamily: "openai", Body: BodyForm, ModelField: "model", ModelLocation: "form", Capability: "transcriptions", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/audio/translations", APIFamily: "openai", Body: BodyForm, ModelField: "model", ModelLocation: "form", Capability: "translations", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/audio/speech", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "speech", Permission: inference},
		{Method: http.MethodGet, Pattern: "/v1/audio/voices", APIFamily: "openai", Body: BodyQuery, ModelField: "model", ModelLocation: "query", Capability: "speech", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/messages", APIFamily: "anthropic", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "anthropic", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/messages/count_tokens", APIFamily: "anthropic", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "anthropic", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v/messages", APIFamily: "anthropic", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "anthropic", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v/messages/count_tokens", APIFamily: "anthropic", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "anthropic", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v2/embed", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "embed", Permission: inference},
		{Method: http.MethodPost, Pattern: "/embed", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "embed", Permission: inference},
		{Method: http.MethodPost, Pattern: "/rerank", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "rerank", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/rerank", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "rerank", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v2/rerank", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "rerank", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/reranking", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "rerank", Permission: inference},
		{Method: http.MethodPost, Pattern: "/reranking", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "rerank", Permission: inference},
		{Method: http.MethodPost, Pattern: "/classify", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "classify", Permission: inference},
		{Method: http.MethodPost, Pattern: "/score", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "score", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/score", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "score", Permission: inference},
		{Method: http.MethodPost, Pattern: "/pooling", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "pooling", Permission: inference},
		{Method: http.MethodPost, Pattern: "/generative_scoring", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "generative_scoring", Permission: inference},
		{Method: http.MethodPost, Pattern: "/infill", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "completions", Permission: inference},
		{Method: http.MethodPost, Pattern: "/completion", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "completions", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/images/generations", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "images", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v1/images/edits", APIFamily: "openai", Body: BodyForm, ModelField: "model", ModelLocation: "form", Capability: "images", Permission: inference},
		{Method: http.MethodGet, Pattern: "/v1/models", Body: BodyQuery, Permission: inference},
		{Method: MethodWebSocket, Pattern: "/v1/realtime", APIFamily: "openai.realtime", Body: BodyWebSocket, ModelField: "session.update.model", ModelLocation: "frame", Capability: "realtime", Streaming: "websocket", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v/chat/completions", APIFamily: "openai", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "chat", Streaming: "sse", Permission: inference},
		{Method: http.MethodPost, Pattern: "/v/responses", APIFamily: "openai.responses", Body: BodyJSON, ModelField: "model", ModelLocation: "body", Capability: "responses", Adapter: AdapterNative, NeedsAffinity: true, Streaming: "sse", Permission: inference},
		{Method: http.MethodGet, Pattern: "/v/responses/{response_id}", APIFamily: "openai.responses", Body: BodyQuery, NeedsAffinity: true, Permission: inference},
		{Method: http.MethodPost, Pattern: "/v/responses/{response_id}/cancel", APIFamily: "openai.responses", Body: BodyQuery, NeedsAffinity: true, Permission: inference},
		{Method: MethodWebSocket, Pattern: "/v/realtime", APIFamily: "openai.realtime", Body: BodyWebSocket, ModelField: "session.update.model", ModelLocation: "frame", Capability: "realtime", Streaming: "websocket", Permission: inference},
	}
}

type Registry struct {
	descriptors []RouteDescriptor
	byKey       map[string]RouteDescriptor
}

func NewRegistry(descriptors []RouteDescriptor) (*Registry, error) {
	r := &Registry{descriptors: make([]RouteDescriptor, 0, len(descriptors)), byKey: make(map[string]RouteDescriptor, len(descriptors))}
	for _, d := range descriptors {
		d.Method = strings.ToUpper(d.Method)
		if err := d.Validate(); err != nil {
			return nil, err
		}
		key := d.Method + " " + d.Pattern
		if _, exists := r.byKey[key]; exists {
			return nil, fmt.Errorf("duplicate route descriptor %s", key)
		}
		r.descriptors = append(r.descriptors, d)
		r.byKey[key] = d
	}
	return r, nil
}

func (r *Registry) List() []RouteDescriptor {
	if r == nil {
		return nil
	}
	out := make([]RouteDescriptor, len(r.descriptors))
	copy(out, r.descriptors)
	return out
}

func (r *Registry) Lookup(method, pattern string) (RouteDescriptor, bool) {
	if r == nil {
		return RouteDescriptor{}, false
	}
	d, ok := r.byKey[strings.ToUpper(method)+" "+pattern]
	return d, ok
}

// Match resolves a concrete request path against the registry. In addition to
// exact patterns it supports the net/http-style placeholders used by the
// descriptors, such as /v1/responses/{response_id} and a final catch-all
// /upstream/{path...}. Returned values are URL-path decoded and keyed by the
// placeholder name. Exact routes are preferred over parameterized routes so a
// future registry entry can safely add a fixed path next to an existing
// wildcard without depending on descriptor order.
func (r *Registry) Match(method, path string) (RouteDescriptor, map[string]string, bool) {
	if r == nil {
		return RouteDescriptor{}, nil, false
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	path = normalizeRoutePath(path)
	for _, descriptor := range r.descriptors {
		if strings.ToUpper(descriptor.Method) != method || strings.Contains(descriptor.Pattern, "{") {
			continue
		}
		if normalizeRoutePath(descriptor.Pattern) == path {
			return descriptor, map[string]string{}, true
		}
	}
	for _, descriptor := range r.descriptors {
		if strings.ToUpper(descriptor.Method) != method || !strings.Contains(descriptor.Pattern, "{") {
			continue
		}
		values, ok := matchRoutePattern(descriptor.Pattern, path)
		if ok {
			return descriptor, values, true
		}
	}
	return RouteDescriptor{}, nil, false
}

func normalizeRoutePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
}

func routePathParts(path string) []string {
	path = normalizeRoutePath(path)
	if path == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

func matchRoutePattern(pattern, path string) (map[string]string, bool) {
	patternParts := routePathParts(pattern)
	pathParts := routePathParts(path)
	values := make(map[string]string)
	for index, patternPart := range patternParts {
		if strings.HasPrefix(patternPart, "{") && strings.HasSuffix(patternPart, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(patternPart, "{"), "}")
			catchAll := strings.HasSuffix(name, "...")
			if catchAll {
				name = strings.TrimSuffix(name, "...")
			}
			if name == "" || strings.ContainsAny(name, "{}") {
				return nil, false
			}
			if catchAll {
				if index > len(pathParts) {
					return nil, false
				}
				raw := strings.Join(pathParts[index:], "/")
				decoded, err := url.PathUnescape(raw)
				if err != nil {
					return nil, false
				}
				values[name] = decoded
				return values, true
			}
			if index >= len(pathParts) {
				return nil, false
			}
			decoded, err := url.PathUnescape(pathParts[index])
			if err != nil {
				return nil, false
			}
			values[name] = decoded
			continue
		}
		if index >= len(pathParts) || patternPart != pathParts[index] {
			return nil, false
		}
	}
	if len(patternParts) != len(pathParts) {
		return nil, false
	}
	return values, true
}
