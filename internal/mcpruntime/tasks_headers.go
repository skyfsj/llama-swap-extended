package mcpruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http/httpguts"
)

type taskToolKey struct{}

func withTaskTool(ctx context.Context, tool *mcp.Tool) context.Context {
	return context.WithValue(ctx, taskToolKey{}, tool)
}

func taskParameterHeaders(tool *mcp.Tool, args any) (http.Header, error) {
	result := http.Header{}
	if tool == nil {
		return result, nil
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if json.Unmarshal(encoded, &schema) != nil {
		return nil, errors.New("invalid MCP tool input schema")
	}
	encoded, err = json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var arguments any
	if err := json.Unmarshal(encoded, &arguments); err != nil {
		return nil, err
	}
	var visit func(map[string]any, any) error
	visit = func(schema map[string]any, value any) error {
		if header, ok := schema["x-mcp-header"].(string); ok && value != nil {
			if !httpguts.ValidHeaderFieldName(header) {
				return errors.New("invalid MCP parameter header name")
			}
			var text string
			switch value := value.(type) {
			case string:
				text = value
			case bool:
				text = strconv.FormatBool(value)
			case float64:
				if value != math.Trunc(value) || math.Abs(value) > 9007199254740991 {
					return errors.New("MCP parameter header requires a safe integer")
				}
				text = strconv.FormatInt(int64(value), 10)
			default:
				return errors.New("MCP parameter header requires a primitive value")
			}
			encode := strings.Trim(text, " \t") != text || strings.HasPrefix(text, "=?base64?") && strings.HasSuffix(text, "?=")
			for _, r := range text {
				if r < 32 || r > 126 {
					encode = true
				}
			}
			if encode {
				text = "=?base64?" + base64.StdEncoding.EncodeToString([]byte(text)) + "?="
			}
			name := "Mcp-Param-" + header
			if _, exists := result[http.CanonicalHeaderKey(name)]; exists {
				return fmt.Errorf("duplicate MCP parameter header %s", header)
			}
			result.Set(name, text)
		}
		properties, _ := schema["properties"].(map[string]any)
		object, _ := value.(map[string]any)
		for name, raw := range properties {
			if child, ok := raw.(map[string]any); ok {
				if err := visit(child, object[name]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(schema, arguments); err != nil {
		return nil, err
	}
	return result, nil
}
