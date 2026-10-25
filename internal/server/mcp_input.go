package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func validateMCPInput(requests mcp.InputRequestMap, responses mcp.InputResponseMap) error {
	if len(requests) == 0 || len(requests) != len(responses) {
		return errors.New("all pending MCP input requests must be answered exactly once")
	}
	for id, request := range requests {
		response, ok := responses[id]
		if !ok || response == nil {
			return fmt.Errorf("missing MCP input response %s", id)
		}
		switch request := request.(type) {
		case *mcp.ElicitParams:
			result, ok := response.(*mcp.ElicitResult)
			if !ok || result.Action != "accept" && result.Action != "decline" && result.Action != "cancel" {
				return errors.New("invalid MCP elicitation response")
			}
			if result.Action == "accept" && request.Mode != "url" && request.RequestedSchema != nil {
				encoded, err := json.Marshal(request.RequestedSchema)
				if err != nil {
					return err
				}
				var schema jsonschema.Schema
				if err := json.Unmarshal(encoded, &schema); err != nil {
					return err
				}
				resolved, err := schema.Resolve(nil)
				if err != nil {
					return err
				}
				if err := resolved.Validate(result.Content); err != nil {
					return fmt.Errorf("MCP input schema: %w", err)
				}
			}
		//lint:ignore SA1019 Validate legacy sampling input during its supported deprecation window.
		case *mcp.CreateMessageParams, *mcp.CreateMessageWithToolsParams:
			switch response.(type) {
			//lint:ignore SA1019 Validate the corresponding legacy sampling result without executing it.
			case *mcp.CreateMessageResult, *mcp.CreateMessageWithToolsResult:
			default:
				return errors.New("invalid MCP sampling response")
			}
		//lint:ignore SA1019 Validate legacy roots input during its supported deprecation window.
		case *mcp.ListRootsParams:
			//lint:ignore SA1019 Roots supplied by the client remain subject to the original MCP request.
			if _, ok := response.(*mcp.ListRootsResult); !ok {
				return errors.New("invalid MCP roots response")
			}
		default:
			return errors.New("unsupported MCP input request")
		}
	}
	return nil
}

func (s *Server) deferMCPInput(ctx context.Context, b mcpruntime.Binding, args any, model string, result *mcp.CallToolResult) error {
	if s.store == nil {
		return errors.New("MCP input continuation requires storage")
	}
	b.CredentialVersion = mcpruntime.ScopeKey(ctx, b.Config)
	b.ConfigurationVersion = mcpConfigurationDigest(s.currentConfig())
	binding, err := json.Marshal(b)
	if err != nil {
		return err
	}
	arguments, err := json.Marshal(args)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	a := store.MCPApproval{ID: protocolObjectID("ginput_"), Owner: identityIDFromContext(ctx), Model: model, Binding: binding, Arguments: arguments, ExpiresAt: time.Now().Add(15 * time.Minute)}
	if err := s.pinMCPBinding(ctx, b, a.ID, a.ExpiresAt); err != nil {
		return err
	}
	if err := s.store.CreateMCPApproval(ctx, a); err != nil {
		mcpruntime.ReleasePin(ctx, a.ID)
		return err
	}
	if err := s.store.ClaimMCPApproval(ctx, a.ID, a.Owner, true); err != nil {
		return err
	}
	if err := s.store.FinishMCPApproval(ctx, a.ID, a.Owner, "completed", encoded); err != nil {
		return err
	}
	return &mcpApprovalRequired{ID: a.ID, InputRequired: true}
}

func (s *Server) pinMCPBinding(ctx context.Context, b mcpruntime.Binding, id string, expires time.Time) error {
	if b.BridgeID != "" {
		return nil
	}
	return mcpruntime.Pin(ctx, b.Config, id, expires)
}
