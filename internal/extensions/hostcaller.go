package extensions

import (
	"context"
	"encoding/json"
	"errors"
)

// HostCaller executes one host-side function on behalf of a running extension
// hook. The worker sends {"type":"call",...} lines over its stdout and awaits
// the result; the host validates every call, so a script can never gain more
// authority than the registry grants.
type HostCaller interface {
	Exec(ctx context.Context, fn string, args json.RawMessage) (any, error)
}

type hostCallerKey struct{}

// WithHostCaller attaches the host-call executor for this request to the
// context. Without one, worker host calls fail with a clear error instead of
// hanging.
func WithHostCaller(ctx context.Context, host HostCaller) context.Context {
	return context.WithValue(ctx, hostCallerKey{}, host)
}

func HostCallerFrom(ctx context.Context) HostCaller {
	host, _ := ctx.Value(hostCallerKey{}).(HostCaller)
	if host == nil {
		return unavailableHost{}
	}
	return host
}

// unavailableHost answers every call with an error so a script awaiting a
// host capability gets a rejection rather than a stall.
type unavailableHost struct{}

func (unavailableHost) Exec(_ context.Context, fn string, _ json.RawMessage) (any, error) {
	return nil, errors.New("host function " + fn + " is unavailable")
}
