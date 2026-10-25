package mcpruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bridgeActorKey struct{}
type bridgeInputEvent struct {
	requests mcp.InputRequestMap
	response chan mcp.InputResponse
}
type bridgeActor struct {
	token       string
	fingerprint string
	events      chan bridgeInputEvent
	done        chan struct{}
	result      mcp.Result
	err         error
	pending     *bridgeInputEvent
	busy        bool
	ctx         context.Context
	cancel      context.CancelFunc
	rounds      int
}

func bridgeRequestFingerprint(method, source string, params mcp.Params) string {
	encoded, _ := json.Marshal(params)
	var raw map[string]any
	_ = json.Unmarshal(encoded, &raw)
	delete(raw, "requestState")
	delete(raw, "inputResponses")
	if meta, ok := raw["_meta"].(map[string]any); ok {
		delete(meta, mcp.MetaKeyProtocolVersion)
		delete(meta, mcp.MetaKeyClientInfo)
		delete(meta, mcp.MetaKeyClientCapabilities)
		if len(meta) == 0 {
			delete(raw, "_meta")
		}
	}
	encoded, _ = json.Marshal(raw)
	return method + "\x00" + source + "\x00" + string(encoded)
}

func bridgeInvoke[T mcp.Result](b *Bridge, ctx context.Context, req mcp.Request, method, source, state string, responses mcp.InputResponseMap, call func(context.Context) (T, error)) (T, error) {
	var zero T
	peer, _ := req.GetSession().(*mcp.ServerSession)
	meta := req.GetParams().GetMeta()
	version, _ := meta[mcp.MetaKeyProtocolVersion].(string)
	if version == "" && peer != nil && peer.InitializeParams() != nil {
		version = peer.InitializeParams().ProtocolVersion
	}
	if version < "2026-07-28" {
		var result T
		err := b.withPeer(ctx, peer, func() error { var err error; result, err = call(ctx); return err })
		return result, err
	}
	fingerprint := bridgeRequestFingerprint(method, source, req.GetParams())
	var actor *bridgeActor
	if strings.HasPrefix(state, "llama-swap-callback:") {
		b.mu.Lock()
		actor = b.actors[state]
		if actor == nil || actor.fingerprint != fingerprint || actor.busy || actor.pending == nil || len(responses) != len(actor.pending.requests) {
			b.mu.Unlock()
			return zero, errors.New("MCP callback continuation does not match a pending request")
		}
		var response mcp.InputResponse
		for id := range actor.pending.requests {
			response = responses[id]
		}
		if response == nil {
			b.mu.Unlock()
			return zero, errors.New("MCP callback input response is missing")
		}
		pending := actor.pending
		actor.pending = nil
		actor.busy = true
		b.mu.Unlock()
		select {
		case pending.response <- response:
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-actor.ctx.Done():
			return zero, actor.ctx.Err()
		}
	} else {
		operation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		stop := context.AfterFunc(b.ctx, cancel)
		actor = &bridgeActor{token: "llama-swap-callback:" + rand.Text(), fingerprint: fingerprint, events: make(chan bridgeInputEvent), done: make(chan struct{}), busy: true, ctx: operation}
		actor.cancel = func() { stop(); cancel() }
		b.mu.Lock()
		if len(b.actors) >= 32 {
			b.mu.Unlock()
			stop()
			cancel()
			return zero, errors.New("MCP callback capacity exceeded")
		}
		b.actors[actor.token] = actor
		b.mu.Unlock()
		context.AfterFunc(operation, func() { b.mu.Lock(); delete(b.actors, actor.token); b.mu.Unlock() })
		go func() {
			defer close(actor.done)
			actor.err = b.withPeer(context.WithValue(operation, bridgeActorKey{}, actor), peer, func() error { result, err := call(operation); actor.result = result; return err })
		}()
	}
	select {
	case event := <-actor.events:
		b.mu.Lock()
		actor.rounds++
		if actor.rounds > 10 {
			b.mu.Unlock()
			actor.cancel()
			return zero, errors.New("MCP callback exceeded the input round limit")
		}
		actor.pending = &event
		actor.busy = false
		b.mu.Unlock()
		encoded, err := json.Marshal(map[string]any{"resultType": "input_required", "inputRequests": event.requests, "requestState": actor.token})
		if err != nil {
			return zero, err
		}
		var result T
		if err := json.Unmarshal(encoded, &result); err != nil {
			return zero, err
		}
		return result, nil
	case <-actor.done:
		return finishBridgeActor[T](b, actor)
	case <-ctx.Done():
		actor.cancel()
		return zero, ctx.Err()
	case <-actor.ctx.Done():
		select {
		case <-actor.done:
			return finishBridgeActor[T](b, actor)
		default:
		}
		return zero, actor.ctx.Err()
	}
}

func finishBridgeActor[T mcp.Result](b *Bridge, actor *bridgeActor) (T, error) {
	var zero T
	b.mu.Lock()
	delete(b.actors, actor.token)
	b.mu.Unlock()
	actor.cancel()
	if actor.err != nil {
		return zero, actor.err
	}
	result, ok := actor.result.(T)
	if !ok {
		return zero, errors.New("MCP callback result type mismatch")
	}
	return result, nil
}

func (b *Bridge) input(ctx context.Context, request mcp.InputRequest) (mcp.InputResponse, bool, error) {
	b.mu.Lock()
	actor := b.actor
	b.mu.Unlock()
	if actor == nil {
		return nil, false, nil
	}
	event := bridgeInputEvent{requests: mcp.InputRequestMap{rand.Text(): request}, response: make(chan mcp.InputResponse)}
	select {
	case actor.events <- event:
	case <-ctx.Done():
		return nil, true, ctx.Err()
	case <-actor.ctx.Done():
		return nil, true, actor.ctx.Err()
	}
	select {
	case response := <-event.response:
		return response, true, nil
	case <-ctx.Done():
		return nil, true, ctx.Err()
	case <-actor.ctx.Done():
		return nil, true, actor.ctx.Err()
	}
}
