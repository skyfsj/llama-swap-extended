package mcpruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bridgeReference struct{ server, original string }

func (b *Bridge) complete(ctx context.Context, req *mcp.CompleteRequest) (result *mcp.CompleteResult, err error) {
	if req.Params == nil || req.Params.Ref == nil || req.Params.Argument.Name == "" {
		return nil, errors.New("MCP completion requires a reference and argument name")
	}
	encoded, err := json.Marshal(req.Params)
	if err != nil || len(encoded) > 1<<20 {
		return nil, errors.New("MCP completion request exceeds capacity")
	}
	err = b.withPeer(ctx, req.Session, func() error {
		params := *req.Params
		ref := *params.Ref
		params.Ref = &ref
		var source bridgeReference
		switch ref.Type {
		case "ref/prompt":
			var ok bool
			source, ok = b.promptOrigins[ref.Name]
			if !ok {
				return errors.New("MCP prompt completion reference is not allowed")
			}
			ref.Name = source.original
		case "ref/resource":
			if !b.templates[ref.URI] && !b.resources[ref.URI] {
				return errors.New("MCP resource completion reference is not allowed")
			}
			id, original, err := b.resourceScope(ref.URI)
			if err != nil {
				return err
			}
			source = bridgeReference{server: id, original: original}
			ref.URI = original
		default:
			return errors.New("unsupported MCP completion reference")
		}
		client := b.clients[source.server]
		if client == nil || client.InitializeResult().Capabilities.Completions == nil {
			return errors.New("MCP upstream does not support completion")
		}
		var err error
		result, err = client.Complete(ctx, &params)
		if err != nil {
			return err
		}
		if result == nil || len(result.Completion.Values) > 100 || result.Completion.Total < 0 || result.Completion.Total > 0 && result.Completion.Total < len(result.Completion.Values) {
			return errors.New("invalid MCP completion result")
		}
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > 1<<20 {
			return errors.New("MCP completion result exceeds capacity")
		}
		return nil
	})
	return
}
