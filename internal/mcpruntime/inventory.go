package mcpruntime

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Inventory struct {
	Scope string
	items int
	bytes int
	names map[string]bool
}

func (i *Inventory) Add(value any) error {
	key := ""
	switch value := value.(type) {
	case *mcp.Tool:
		if value == nil || value.Name == "" || len(value.Name) > 128 {
			return errors.New("invalid MCP tool name")
		}
		key = "tool:" + value.Name
	case *mcp.Prompt:
		if value == nil || value.Name == "" || len(value.Name) > 128 {
			return errors.New("invalid MCP prompt name")
		}
		key = "prompt:" + value.Name
	case *mcp.Resource:
		if value == nil || value.URI == "" || len(value.URI) > 16<<10 {
			return errors.New("invalid MCP resource URI")
		}
		key = "resource:" + value.URI
	case *mcp.ResourceTemplate:
		if value == nil || value.URITemplate == "" || len(value.URITemplate) > 16<<10 {
			return errors.New("invalid MCP resource template")
		}
		key = "template:" + value.URITemplate
	default:
		return errors.New("unsupported MCP inventory entry")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 || i.items >= 4096 || i.bytes+len(encoded) > 8<<20 {
		return errors.New("MCP inventory exceeds capacity")
	}
	if i.names == nil {
		i.names = map[string]bool{}
	}
	key = i.Scope + "\x00" + key
	if key != "" && i.names[key] {
		return errors.New("MCP inventory contains duplicate identities")
	}
	i.names[key] = true
	i.items++
	i.bytes += len(encoded)
	return nil
}
func RegisterTool(server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandler) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("invalid MCP tool definition")
		}
	}()
	server.AddTool(tool, handler)
	return nil
}
func RegisterResourceTemplate(server *mcp.Server, template *mcp.ResourceTemplate, handler mcp.ResourceHandler) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("invalid MCP resource template")
		}
	}()
	server.AddResourceTemplate(template, handler)
	return nil
}
