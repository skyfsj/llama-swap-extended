package swaputil

import (
	"context"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/conversation"
)

type UpstreamHooks struct {
	Prepare  func(context.Context, conversation.Scope, *http.Request) error
	Complete func(context.Context, conversation.Scope, []byte, []byte, http.Header, int, bool)
}

type upstreamHooksKey struct{}

func WithUpstreamHooks(ctx context.Context, hooks UpstreamHooks) context.Context {
	previous := UpstreamHooksFrom(ctx)
	combined := UpstreamHooks{Prepare: func(ctx context.Context, scope conversation.Scope, r *http.Request) error {
		if hooks.Prepare != nil {
			if err := hooks.Prepare(ctx, scope, r); err != nil {
				return err
			}
		}
		if previous.Prepare != nil {
			return previous.Prepare(ctx, scope, r)
		}
		return nil
	}, Complete: func(ctx context.Context, scope conversation.Scope, in, out []byte, h http.Header, status int, truncated bool) {
		if hooks.Complete != nil {
			hooks.Complete(ctx, scope, in, out, h, status, truncated)
		}
		if previous.Complete != nil {
			previous.Complete(ctx, scope, in, out, h, status, truncated)
		}
	}}
	return context.WithValue(ctx, upstreamHooksKey{}, combined)
}

type upstreamHTTPClientKey struct{}

func WithUpstreamHTTPClient(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, upstreamHTTPClientKey{}, client)
}
func UpstreamHTTPClient(ctx context.Context) *http.Client {
	client, _ := ctx.Value(upstreamHTTPClientKey{}).(*http.Client)
	return client
}
func UpstreamHooksFrom(ctx context.Context) UpstreamHooks {
	hooks, _ := ctx.Value(upstreamHooksKey{}).(UpstreamHooks)
	return hooks
}
func WithoutUpstreamHooks(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamHooksKey{}, UpstreamHooks{})
}
