// Package backend defines the stable boundary between route/protocol logic and
// an inference runtime. Adapters are intentionally small: lifecycle and
// process scheduling stay in router/runtime packages, while protocol-specific
// capabilities live here.
package backend

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"
)

type CapabilitySet map[string]bool

func (c CapabilitySet) Has(name string) bool { return c != nil && c[name] }

type CacheState struct {
	Supported    bool   `json:"supported"`
	Sleeping     bool   `json:"sleeping"`
	CachedTokens int64  `json:"cachedTokens"`
	LastReset    string `json:"lastReset,omitempty"`
}

// Normalize bounds cache telemetry supplied by an adapter before it is
// exposed through the control plane. Optional backends are not trusted to
// produce valid counters or display-safe reset markers.
func (s CacheState) Normalize() CacheState {
	if s.CachedTokens < 0 {
		s.CachedTokens = 0
	}
	s.LastReset = normalizeProgressText(s.LastReset, 128)
	return s
}

// Normalize bounds the local cache observation kept by CacheController. The
// prefix hash is diagnostic identity only; malformed/invisible values are
// dropped rather than copied into UI or audit payloads.
func (r CacheReport) Normalize() CacheReport {
	if r.CachedTokens < 0 {
		r.CachedTokens = 0
	}
	if r.CreationTokens < 0 {
		r.CreationTokens = 0
	}
	r.PrefixHash = normalizeProgressText(r.PrefixHash, 256)
	if r.ObservedAt.IsZero() {
		r.ObservedAt = time.Now()
	}
	return r
}

type Progress struct {
	Phase     string `json:"phase"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

type RequestTransform struct {
	Body   []byte
	Header http.Header
}

// BackendAdapter is the only interface a route needs to know about. An
// adapter may return ErrUnsupported for optional cache/sleep operations; the
// control plane then reports a capability-aware 501 instead of proxying an
// arbitrary management path.
type BackendAdapter interface {
	Name() string
	Capabilities(context.Context) (CapabilitySet, error)
	TransformRequest(context.Context, string, RequestTransform) (RequestTransform, error)
	TransformResponse(context.Context, string, []byte, http.Header) ([]byte, error)
	CacheState(context.Context) (CacheState, error)
	ResetCache(context.Context) error
	Sleep(context.Context, int) error
	Wake(context.Context) error
	Progress(context.Context) (Progress, error)
}

var ErrUnsupported = errors.New("backend capability is not supported")

// isNilAdapter handles both a nil interface and an interface containing a
// typed-nil pointer. The latter is easy to create when optional adapters are
// assembled by embedders and otherwise reaches method calls that use value
// receivers, causing a control-plane panic instead of a capability error.
func isNilAdapter(adapter BackendAdapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// IsNilAdapter exposes the typed-nil check to middleware that stores adapters
// in a map outside this package. It is intentionally a predicate only; it
// does not invoke any adapter method.
func IsNilAdapter(adapter BackendAdapter) bool { return isNilAdapter(adapter) }

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]BackendAdapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]BackendAdapter)} }

func (r *Registry) Register(name string, adapter BackendAdapter) error {
	if r == nil || isNilAdapter(adapter) || name == "" {
		return errors.New("backend name and adapter are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adapters == nil {
		r.adapters = make(map[string]BackendAdapter)
	}
	if _, exists := r.adapters[name]; exists {
		return errors.New("backend adapter already registered")
	}
	r.adapters[name] = adapter
	return nil
}

func (r *Registry) Get(name string) (BackendAdapter, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}
