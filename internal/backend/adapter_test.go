package backend

import (
	"context"
	"net/http"
	"testing"
)

type nilUnsafeAdapter struct{}

func (*nilUnsafeAdapter) Name() string { return "nil-unsafe" }
func (*nilUnsafeAdapter) Capabilities(context.Context) (CapabilitySet, error) {
	return nil, nil
}
func (*nilUnsafeAdapter) TransformRequest(context.Context, string, RequestTransform) (RequestTransform, error) {
	return RequestTransform{}, nil
}
func (*nilUnsafeAdapter) TransformResponse(context.Context, string, []byte, http.Header) ([]byte, error) {
	return nil, nil
}
func (*nilUnsafeAdapter) CacheState(context.Context) (CacheState, error) { return CacheState{}, nil }
func (*nilUnsafeAdapter) ResetCache(context.Context) error               { return nil }
func (*nilUnsafeAdapter) Sleep(context.Context, int) error               { return nil }
func (*nilUnsafeAdapter) Wake(context.Context) error                     { return nil }
func (*nilUnsafeAdapter) Progress(context.Context) (Progress, error)     { return Progress{}, nil }

func TestBackend_TypedNilAdapterIsRejected(t *testing.T) {
	var typedNil *nilUnsafeAdapter
	var adapter BackendAdapter = typedNil
	if !IsNilAdapter(adapter) {
		t.Fatal("typed-nil adapter was not detected")
	}
	if IsNilAdapter(Passthrough{ID: "ok"}) {
		t.Fatal("non-nil value adapter was rejected")
	}
	registry := NewRegistry()
	if err := registry.Register("nil", adapter); err == nil {
		t.Fatal("registry accepted typed-nil adapter")
	}
	controller := NewCacheController()
	controller.Register("nil", adapter)
	if _, err := controller.State(context.Background(), "nil"); err == nil {
		t.Fatal("cache controller retained typed-nil adapter")
	}
}
