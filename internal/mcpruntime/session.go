package mcpruntime

import (
	"context"
	"errors"
	"iter"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type operationTokenKey struct{}
type operationToken struct{ active bool }

func (s *Session) operation(ctx context.Context) (context.Context, func(error)) {
	ctx = context.WithValue(ctx, operationTokenKey{}, &operationToken{active: true})
	operation := ctx
	finish := func() {}
	if s.manager != nil {
		operation, finish = s.manager.operation(ctx)
	} else if s.lifetime != nil {
		var cancel context.CancelFunc
		operation, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(s.lifetime, cancel)
		finish = func() { stop(); cancel() }
	}
	return operation, func(err error) {
		if s.httpCancellation != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.httpCancellation.completeOperation(operation)
		}
		finish()
	}
}
func sessionCall[T any](s *Session, ctx context.Context, call func(context.Context) (T, error)) (T, error) {
	operation, finish := s.operation(ctx)
	result, err := call(operation)
	finish(err)
	return result, err
}
func (s *Session) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if params != nil {
		copy := *params
		copy.Meta = ForwardMeta(params.Meta)
		params = &copy
	}
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.CallToolResult, error) { return s.ClientSession.CallTool(ctx, params) })
}
func (s *Session) ReadResource(ctx context.Context, params *mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	if params != nil {
		copy := *params
		copy.Meta = ForwardMeta(params.Meta)
		params = &copy
	}
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.ReadResourceResult, error) {
		return s.ClientSession.ReadResource(ctx, params)
	})
}
func (s *Session) GetPrompt(ctx context.Context, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	if params != nil {
		copy := *params
		copy.Meta = ForwardMeta(params.Meta)
		params = &copy
	}
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.GetPromptResult, error) { return s.ClientSession.GetPrompt(ctx, params) })
}

func (s *Session) Complete(ctx context.Context, params *mcp.CompleteParams) (*mcp.CompleteResult, error) {
	if params != nil {
		copy := *params
		copy.Meta = ForwardMeta(params.Meta)
		params = &copy
	}
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.CompleteResult, error) { return s.ClientSession.Complete(ctx, params) })
}

func ForwardMeta(meta mcp.Meta) mcp.Meta {
	copy := mcp.Meta{}
	for key, value := range meta {
		if key != mcp.MetaKeyProtocolVersion && key != mcp.MetaKeyClientInfo && key != mcp.MetaKeyClientCapabilities {
			copy[key] = value
		}
	}
	return copy
}
func (s *Session) ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.ListToolsResult, error) { return s.ClientSession.ListTools(ctx, params) })
}
func (s *Session) ListResources(ctx context.Context, params *mcp.ListResourcesParams) (*mcp.ListResourcesResult, error) {
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.ListResourcesResult, error) {
		return s.ClientSession.ListResources(ctx, params)
	})
}
func (s *Session) ListResourceTemplates(ctx context.Context, params *mcp.ListResourceTemplatesParams) (*mcp.ListResourceTemplatesResult, error) {
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.ListResourceTemplatesResult, error) {
		return s.ClientSession.ListResourceTemplates(ctx, params)
	})
}
func (s *Session) ListPrompts(ctx context.Context, params *mcp.ListPromptsParams) (*mcp.ListPromptsResult, error) {
	return sessionCall(s, ctx, func(ctx context.Context) (*mcp.ListPromptsResult, error) {
		return s.ClientSession.ListPrompts(ctx, params)
	})
}
func sessionIter[T any](s *Session, ctx context.Context, sequence func(context.Context) iter.Seq2[T, error]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		operation, finish := s.operation(ctx)
		var failure error
		defer func() { finish(failure) }()
		for value, err := range sequence(operation) {
			failure = err
			if !yield(value, err) {
				return
			}
		}
	}
}
func (s *Session) Tools(ctx context.Context, params *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error] {
	return sessionIter(s, ctx, func(ctx context.Context) iter.Seq2[*mcp.Tool, error] { return s.ClientSession.Tools(ctx, params) })
}
func (s *Session) Resources(ctx context.Context, params *mcp.ListResourcesParams) iter.Seq2[*mcp.Resource, error] {
	return sessionIter(s, ctx, func(ctx context.Context) iter.Seq2[*mcp.Resource, error] {
		return s.ClientSession.Resources(ctx, params)
	})
}
func (s *Session) ResourceTemplates(ctx context.Context, params *mcp.ListResourceTemplatesParams) iter.Seq2[*mcp.ResourceTemplate, error] {
	return sessionIter(s, ctx, func(ctx context.Context) iter.Seq2[*mcp.ResourceTemplate, error] {
		return s.ClientSession.ResourceTemplates(ctx, params)
	})
}
func (s *Session) Prompts(ctx context.Context, params *mcp.ListPromptsParams) iter.Seq2[*mcp.Prompt, error] {
	return sessionIter(s, ctx, func(ctx context.Context) iter.Seq2[*mcp.Prompt, error] { return s.ClientSession.Prompts(ctx, params) })
}
