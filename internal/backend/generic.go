package backend

import (
	"context"
	"net/http"
)

// Passthrough implements a safe native adapter for legacy cmd/proxy models.
// Optional runtime controls remain unsupported rather than being guessed.
type Passthrough struct {
	ID   string
	Caps CapabilitySet
}

func (p Passthrough) Name() string                                        { return p.ID }
func (p Passthrough) Capabilities(context.Context) (CapabilitySet, error) { return p.Caps, nil }
func (p Passthrough) TransformRequest(_ context.Context, _ string, req RequestTransform) (RequestTransform, error) {
	return req, nil
}
func (p Passthrough) TransformResponse(_ context.Context, _ string, body []byte, _ http.Header) ([]byte, error) {
	return body, nil
}
func (p Passthrough) CacheState(context.Context) (CacheState, error) {
	return CacheState{}, ErrUnsupported
}
func (p Passthrough) ResetCache(context.Context) error           { return ErrUnsupported }
func (p Passthrough) Sleep(context.Context, int) error           { return ErrUnsupported }
func (p Passthrough) Wake(context.Context) error                 { return ErrUnsupported }
func (p Passthrough) Progress(context.Context) (Progress, error) { return Progress{Phase: "idle"}, nil }
