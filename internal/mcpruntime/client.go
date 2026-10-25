package mcpruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

type headerTransport struct {
	base         http.RoundTripper
	origin       *url.URL
	headers      http.Header
	cancellation *cancellationConnection
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.EqualFold(r.URL.Host, t.origin.Host) || r.URL.Scheme != t.origin.Scheme {
		return nil, errors.New("MCP endpoint changed origin")
	}
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	for k, v := range t.headers {
		r.Header[k] = append([]string(nil), v...)
	}
	if err := rewriteTaskHTTP(r); err != nil {
		return nil, err
	}
	finish := t.cancellation.httpRequest(r)
	response, err := t.base.RoundTrip(r)
	finish(response, err)
	return response, err
}

func Connect(ctx context.Context, cfg config.MCPServerConfig) (*Session, error) {
	if scope, ok := ctx.Value(managerContextKey{}).(managerContext); ok && scope.manager != nil {
		return scope.manager.acquire(ctx, scope.owner, cfg)
	}
	return connectSession(ctx, cfg, ctx)
}

func connectSession(ctx context.Context, cfg config.MCPServerConfig, lifetime context.Context) (*Session, error) {
	return connectSessionOptions(ctx, cfg, lifetime, nil, nil)
}

func connectSessionOptions(ctx context.Context, cfg config.MCPServerConfig, lifetime context.Context, clientOptions *mcp.ClientOptions, receiving mcp.Middleware) (*Session, error) {
	if err := (config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Servers: map[string]config.MCPServerConfig{"server": cfg}}}).Validate(); err != nil {
		return nil, err
	}
	var transport mcp.Transport
	var httpCancellation *cancellationConnection
	switch cfg.Transport {
	case "stdio":
		cmd := exec.CommandContext(lifetime, cfg.Command, cfg.Args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for _, name := range cfg.Env {
			value, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("MCP environment variable %s is unset", name)
			}
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		transport = &mcp.CommandTransport{Command: cmd}
		transport = taskAliasTransport{transport}
	case "http", "sse":
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return nil, err
		}
		headers := http.Header{}
		for header, name := range cfg.Headers {
			value, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("MCP credential environment variable %s is unset", name)
			}
			headers.Set(header, value)
		}
		httpCancellation = &cancellationConnection{pending: map[string]*pendingCancellation{}}
		client := &http.Client{Transport: headerTransport{base: http.DefaultTransport, origin: u, headers: headers, cancellation: httpCancellation}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if cfg.Transport == "http" {
			transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: client, MaxRetries: 0, MaxEventSize: 16 << 20}
		} else {
			transport = &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: client, MaxEventSize: 16 << 20}
		}
	default:
		return nil, errors.New("unsupported MCP transport")
	}
	var options *mcp.ClientSessionOptions
	var pinned *lifetimeTransport
	if cfg.Transport == "sse" {
		pinned = &lifetimeTransport{Transport: transport, lifetime: lifetime}
		transport = pinned
	}
	var cancellation *cancellationTransport
	if cfg.Transport == "stdio" || cfg.Transport == "sse" {
		options = &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"}
		cancellation = &cancellationTransport{Transport: transport}
		transport = cancellation
	}
	if clientOptions == nil {
		clientOptions = &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "llama-swap", Version: "1"}, clientOptions)
	if err := registerTaskMethods(client); err != nil {
		return nil, err
	}
	if receiving != nil {
		client.AddReceivingMiddleware(receiving)
	}
	session, err := client.Connect(ctx, transport, options)
	if err != nil {
		if pinned != nil && pinned.cancel != nil {
			pinned.cancel()
		}
		return nil, err
	}
	if httpCancellation != nil {
		httpCancellation.completeOperation(ctx)
	}
	var closeTransport context.CancelFunc
	if pinned != nil {
		closeTransport = pinned.cancel
	}
	return &Session{ClientSession: session, cancellation: cancellation, httpCancellation: httpCancellation, closeTransport: closeTransport, lifetime: lifetime}, nil
}

func BoundName(server, tool string) string {
	raw := server + "\x00" + tool
	sum := sha256.Sum256([]byte(raw))
	prefix := "mcp__" + server + "__"
	for _, c := range tool {
		if len(prefix) >= 47 {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			prefix += string(c)
		} else {
			prefix += "_"
		}
	}
	if len(prefix) > 47 {
		prefix = prefix[:47]
	}
	return prefix + "_" + hex.EncodeToString(sum[:8])
}

func Allowed(list []string, value string) bool {
	if len(list) == 0 {
		return true
	}
	for _, v := range list {
		if v == value || v == "*" {
			return true
		}
	}
	return false
}

type Binding struct {
	Server               string
	Config               config.MCPServerConfig
	Tool                 *mcp.Tool
	Name                 string
	Continuation         *mcp.CallToolParams `json:"continuation,omitempty"`
	CredentialVersion    string              `json:"credential_version,omitempty"`
	ConfigurationVersion string              `json:"configuration_version,omitempty"`
	RequireApproval      bool                `json:"require_approval,omitempty"`
	BridgeID             string              `json:"bridge_id,omitempty"`
}

func Discover(ctx context.Context, cfg config.MCPRuntimeConfig, model string) ([]Binding, error) {
	var bindings []Binding
	totalBytes := 0
	for id, server := range cfg.Servers {
		if !Allowed(server.Models, model) {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		session, err := Connect(ctx, server)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("MCP %s: %w", id, err)
		}
		if session.InitializeResult().Capabilities.Tools != nil {
			inventory := Inventory{}
			for tool, err := range session.Tools(ctx, nil) {
				if err != nil {
					session.Close()
					cancel()
					return nil, err
				}
				if err := inventory.Add(tool); err != nil {
					session.Close()
					cancel()
					return nil, err
				}
				wire, _ := json.Marshal(tool)
				totalBytes += len(wire)
				if totalBytes > 8<<20 || len(bindings) >= 4096 {
					session.Close()
					cancel()
					return nil, errors.New("MCP tool inventory exceeds capacity")
				}
				if !Allowed(server.Tools, tool.Name) {
					continue
				}
				schema, err := json.Marshal(tool.InputSchema)
				if err != nil {
					session.Close()
					cancel()
					return nil, err
				}
				var object map[string]any
				if json.Unmarshal(schema, &object) != nil || object["type"] != "object" {
					session.Close()
					cancel()
					return nil, fmt.Errorf("MCP %s tool %s has invalid input schema", id, tool.Name)
				}
				bindings = append(bindings, Binding{Server: id, Config: server, Tool: tool, Name: BoundName(id, tool.Name)})
			}
		}
		session.Close()
		cancel()
	}
	return bindings, nil
}

func (b Binding) Call(ctx context.Context, args any) (*mcp.CallToolResult, error) {
	if b.Continuation != nil {
		params := *b.Continuation
		params.Arguments = args
		return b.CallParams(ctx, &params)
	}
	return b.CallParams(ctx, &mcp.CallToolParams{Name: b.Tool.Name, Arguments: args})
}
func (b Binding) CallParams(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := params.Arguments
	schemaJSON, err := json.Marshal(b.Tool.InputSchema)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil, err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, err
	}
	if err := resolved.Validate(args); err != nil {
		return nil, fmt.Errorf("MCP tool arguments: %w", err)
	}
	session, err := Connect(ctx, b.Config)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	copy := *params
	copy.Name = b.Tool.Name
	return session.CallTool(ctx, &copy)
}
