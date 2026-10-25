package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func mcpNativeToolResult(endpoint string, call extensionCall, output map[string]any) (map[string]any, error) {
	raw, ok := output["mcp_result"]
	if !ok {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	parts := []dialect.Part{}
	for _, content := range result.Content {
		converted, err := mcpContextParts(content)
		if err != nil {
			return nil, err
		}
		parts = append(parts, converted...)
	}
	parts = append(parts, dialect.Part{Kind: dialect.PartText, Text: string(encoded)})
	format := dialect.OpenAIResponses
	switch endpoint {
	case "chat.completions":
		format = dialect.OpenAIChat
	case "anthropic.messages":
		format = dialect.ClaudeMessages
	case "gemini.generateContent":
		format = dialect.Gemini
	}
	request := &dialect.Request{Model: "mcp-result", Messages: []dialect.Message{{Role: dialect.RoleTool, Parts: []dialect.Part{{Kind: dialect.PartToolResult, ToolCallID: call.ID, ToolName: call.Name, IsError: result.IsError, Content: parts}}}}}
	body, diagnostics, err := dialect.EncodeRequest(request, format, dialect.ConvertOptions{})
	if err != nil {
		return nil, err
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Code, "dropped") {
			return nil, fmt.Errorf("MCP result cannot be represented on %s: %s", endpoint, diagnostic.Code)
		}
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	switch format {
	case dialect.OpenAIResponses:
		items, _ := parsed["input"].([]any)
		if len(items) > 0 {
			item, _ := items[0].(map[string]any)
			return item, nil
		}
	case dialect.OpenAIChat:
		items, _ := parsed["messages"].([]any)
		if len(items) > 0 {
			item, _ := items[0].(map[string]any)
			return item, nil
		}
	case dialect.ClaudeMessages:
		messages, _ := parsed["messages"].([]any)
		if len(messages) > 0 {
			message, _ := messages[0].(map[string]any)
			content, _ := message["content"].([]any)
			if len(content) > 0 {
				block, _ := content[0].(map[string]any)
				return block, nil
			}
		}
	case dialect.Gemini:
		contents, _ := parsed["contents"].([]any)
		if len(contents) > 0 {
			content, _ := contents[0].(map[string]any)
			parts, _ := content["parts"].([]any)
			if len(parts) > 0 {
				part, _ := parts[0].(map[string]any)
				return map[string]any{"type": "function_response", "name": call.Name, "native": part["functionResponse"]}, nil
			}
		}
	}
	return nil, fmt.Errorf("MCP result encoder produced no %s tool result", endpoint)
}
