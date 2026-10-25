package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type mcpContextSelection struct {
	Resources []struct {
		Server string `json:"server"`
		URI    string `json:"uri"`
	} `json:"resources"`
	Prompts []struct {
		Server    string            `json:"server"`
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	} `json:"prompts"`
}

func mcpContextParts(content mcp.Content) ([]dialect.Part, error) {
	switch value := content.(type) {
	case *mcp.TextContent:
		return []dialect.Part{{Kind: dialect.PartText, Text: value.Text}}, nil
	case *mcp.ImageContent:
		return []dialect.Part{{Kind: dialect.PartImage, MediaType: value.MIMEType, Base64: base64.StdEncoding.EncodeToString(value.Data)}}, nil
	case *mcp.AudioContent:
		return []dialect.Part{{Kind: dialect.PartAudio, MediaType: value.MIMEType, Base64: base64.StdEncoding.EncodeToString(value.Data)}}, nil
	case *mcp.EmbeddedResource:
		return mcpResourceParts(value.Resource)
	case *mcp.ResourceLink:
		return []dialect.Part{{Kind: dialect.PartText, Text: fmt.Sprintf("Resource: %s\n%s\n%s", value.Name, value.URI, value.Description)}}, nil
	default:
		return nil, errors.New("MCP content type cannot be injected into model context")
	}
}
func mcpResourceParts(resource *mcp.ResourceContents) ([]dialect.Part, error) {
	if resource == nil {
		return nil, errors.New("empty MCP resource")
	}
	parts := []dialect.Part{{Kind: dialect.PartText, Text: "MCP resource: " + resource.URI}}
	if resource.Text != "" {
		parts = append(parts, dialect.Part{Kind: dialect.PartText, Text: resource.Text})
	}
	if len(resource.Blob) > 0 {
		kind := dialect.PartDocument
		if strings.HasPrefix(resource.MIMEType, "image/") {
			kind = dialect.PartImage
		} else if strings.HasPrefix(resource.MIMEType, "audio/") {
			kind = dialect.PartAudio
		} else if strings.HasPrefix(resource.MIMEType, "video/") {
			kind = dialect.PartVideo
		}
		parts = append(parts, dialect.Part{Kind: kind, MediaType: resource.MIMEType, Base64: base64.StdEncoding.EncodeToString(resource.Blob)})
	}
	return parts, nil
}

func createMCPContextMiddleware(s *Server, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.ProtocolRuntime.MCP.Enabled {
				r = r.WithContext(s.mcpContext(r.Context()))
			}
			route, ok := dialect.RouteForRequest(r.Method, r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			body, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, 413, err.Error())
				return
			}
			var raw map[string]json.RawMessage
			if json.Unmarshal(body, &raw) != nil {
				swaputil.SendResponse(w, r, 400, "invalid JSON request")
				return
			}
			r = r.WithContext(withMCPRequestPolicy(r.Context(), cfg, raw))
			selection, exists := raw["gateway_context"]
			if !exists {
				setBackendRequestBody(r, body)
				next.ServeHTTP(w, r)
				return
			}
			if !cfg.ProtocolRuntime.MCP.Enabled {
				swaputil.SendResponse(w, r, 400, "gateway MCP context requires the MCP runtime")
				return
			}
			var chosen mcpContextSelection
			if json.Unmarshal(selection, &chosen) != nil || len(chosen.Resources)+len(chosen.Prompts) > 32 {
				swaputil.SendResponse(w, r, 400, "invalid MCP context selection")
				return
			}
			data, ok := swaputil.ReadContext(r.Context())
			if !ok {
				swaputil.SendResponse(w, r, 500, "model context is unavailable")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			defer cancel()
			messages := []dialect.Message{}
			connect := func(server string) (*mcpruntime.Session, config.MCPServerConfig, error) {
				remote, ok := cfg.ProtocolRuntime.MCP.Servers[server]
				if !ok || !mcpruntime.Allowed(remote.Models, data.ModelID) {
					return nil, remote, errors.New("MCP server is not allowed for this model")
				}
				session, err := mcpruntime.Connect(ctx, remote)
				return session, remote, err
			}
			for _, ref := range chosen.Resources {
				session, remote, err := connect(ref.Server)
				if err != nil {
					swaputil.SendResponse(w, r, 400, err.Error())
					return
				}
				if !mcpruntime.Allowed(remote.Resources, ref.URI) {
					session.Close()
					swaputil.SendResponse(w, r, 403, "MCP resource is not allowed")
					return
				}
				result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: ref.URI})
				session.Close()
				if err != nil {
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if result.NeedsInput() {
					swaputil.SendResponse(w, r, 409, "MCP context resource requires client input through /mcp")
					return
				}
				message := dialect.Message{Role: dialect.RoleUser}
				for _, resource := range result.Contents {
					parts, err := mcpResourceParts(resource)
					if err != nil {
						swaputil.SendResponse(w, r, 502, err.Error())
						return
					}
					message.Parts = append(message.Parts, parts...)
				}
				messages = append(messages, message)
			}
			for _, ref := range chosen.Prompts {
				session, remote, err := connect(ref.Server)
				if err != nil {
					swaputil.SendResponse(w, r, 400, err.Error())
					return
				}
				if !mcpruntime.Allowed(remote.Prompts, ref.Name) {
					session.Close()
					swaputil.SendResponse(w, r, 403, "MCP prompt is not allowed")
					return
				}
				result, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: ref.Name, Arguments: ref.Arguments})
				session.Close()
				if err != nil {
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if result.NeedsInput() {
					swaputil.SendResponse(w, r, 409, "MCP context prompt requires client input through /mcp")
					return
				}
				for _, message := range result.Messages {
					if message == nil || message.Role != "user" && message.Role != "assistant" {
						swaputil.SendResponse(w, r, 502, "MCP prompt returned an unsupported message role")
						return
					}
					parts, err := mcpContextParts(message.Content)
					if err != nil {
						swaputil.SendResponse(w, r, 400, err.Error())
						return
					}
					role := dialect.RoleUser
					if message.Role == "assistant" {
						role = dialect.RoleAssistant
					}
					messages = append(messages, dialect.Message{Role: role, Parts: parts})
				}
			}
			encoded, diagnostics, err := dialect.EncodeRequest(&dialect.Request{Model: data.ModelID, Messages: messages}, route.Format, dialect.ConvertOptions{})
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			for _, d := range diagnostics {
				if strings.Contains(d.Code, "dropped") {
					swaputil.SendResponse(w, r, 422, "MCP context cannot be represented: "+d.Code)
					return
				}
			}
			var injected map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &injected)
			key := "messages"
			switch route.Format {
			case dialect.Gemini:
				key = "contents"
			case dialect.OpenAIResponses, dialect.GeminiInteractions:
				key = "input"
			}
			var prefix []json.RawMessage
			_ = json.Unmarshal(injected[key], &prefix)
			current, err := conversation.NormalizeInput(route.Format, raw[key])
			if err != nil {
				swaputil.SendResponse(w, r, 400, err.Error())
				return
			}
			raw[key], _ = json.Marshal(append(prefix, current...))
			delete(raw, "gateway_context")
			body, err = json.Marshal(raw)
			if err != nil || len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendResponse(w, r, 413, "MCP context exceeds request capacity")
				return
			}
			setBackendRequestBody(r, body)
			next.ServeHTTP(w, r)
		})
	}
}
