package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/docagent"
	"gopkg.in/yaml.v3"
)

// MCP endpoint at /api/mcp. It serves llama-swap's own documentation and a
// read-only view of the running configuration to an MCP client (Claude Code,
// Codex, any agent host).
//
// Two protocol revisions are accepted, because both are in the wild:
//
//	2026-07-28  stateless: server/discover, plus Mcp-Protocol-Version and
//	            Mcp-Method headers, no initialize handshake
//	2025-06-18  the classic initialize / notifications/initialized handshake
//
// Supporting both is a superset: a modern client opens with server/discover and
// never sends initialize, an older one sends initialize and never discovers.
// Answering only one shape would lock out half the clients for no benefit —
// every tool call is identical under both.
const (
	mcpProtocolVersionModern = "2026-07-28"
	mcpProtocolVersionLegacy = "2025-06-18"

	headerMCPProtocolVersion = "Mcp-Protocol-Version"
	headerMCPMethod          = "Mcp-Method"
)

// JSON-RPC 2.0 error codes, plus the MCP-specific version code from SEP-2575.
const (
	codeMCPParseError              = -32700
	codeMCPInvalidRequest          = -32600
	codeMCPMethodNotFound          = -32601
	codeMCPInvalidParams           = -32602
	codeMCPUnsupportedProtocolCode = -32022
)

// maxMCPBodyBytes bounds the request body. MCP tool arguments are a small JSON
// object; anything larger is a mistake or an attack.
const maxMCPBodyBytes = 64 * 1024

// toolListTTLMs tells a client how often to re-list tools. The endpoint cannot
// send notifications/tools/list_changed, so the TTL is what keeps a client's
// list from going stale after an upgrade.
const toolListTTLMs = 60_000

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// handleAPIMCP serves the documentation tools as a stateless MCP server.
func (s *Server) handleAPIMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// RFC 9110 15.5.6: a 405 must say what is allowed.
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBodyBytes+1))
	if err != nil {
		writeMCPError(w, json.RawMessage("null"), codeMCPParseError, "could not read request body", nil)
		return
	}
	if len(body) > maxMCPBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		// JSON-RPC batching was removed from MCP in revision 2025-06-18.
		writeMCPError(w, json.RawMessage("null"), codeMCPInvalidRequest, "JSON-RPC batching is not supported", nil)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeMCPError(w, json.RawMessage("null"), codeMCPParseError, "request is not a JSON-RPC message", nil)
		return
	}

	// The version header is checked when a client sends one. A client that
	// omits it is speaking the handshake revision by definition, so it is not
	// rejected for the omission.
	if version := strings.TrimSpace(r.Header.Get(headerMCPProtocolVersion)); version != "" && !mcpVersionSupported(version) {
		data, _ := json.Marshal(map[string]any{
			"supported": []string{mcpProtocolVersionModern, mcpProtocolVersionLegacy},
			"requested": version,
		})
		writeMCPError(w, req.ID, codeMCPUnsupportedProtocolCode,
			fmt.Sprintf("unsupported protocol version; this server speaks MCP %s and %s",
				mcpProtocolVersionModern, mcpProtocolVersionLegacy), data)
		return
	}
	// When the client does send the routing header it must agree with the body,
	// or a proxy and this server would disagree about what was called.
	if header := r.Header.Get(headerMCPMethod); header != "" && header != req.Method {
		writeMCPError(w, req.ID, codeMCPInvalidRequest,
			fmt.Sprintf("header mismatch: %s is %q but the request method is %q", headerMCPMethod, header, req.Method), nil)
		return
	}

	// A notification carries no id and expects no response body.
	if isMCPNotification(req.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	provider := s.mcpProvider
	if provider == nil {
		writeMCPError(w, req.ID, codeMCPMethodNotFound, "documentation tools are not available in this build", nil)
		return
	}

	switch req.Method {
	case "initialize":
		writeMCPResult(w, req.ID, s.mcpInitializeResult(req.Params))
	case "server/discover":
		writeMCPResult(w, req.ID, s.mcpDiscoverResult())
	case "tools/list":
		writeMCPResult(w, req.ID, map[string]any{
			"tools":      provider.Tools(),
			"ttlMs":      toolListTTLMs,
			"cacheScope": "public",
		})
	case "tools/call":
		s.handleMCPToolCall(w, r, req, provider)
	case "ping":
		writeMCPResult(w, req.ID, map[string]any{})
	default:
		writeMCPError(w, req.ID, codeMCPMethodNotFound, fmt.Sprintf("unsupported method %q", req.Method), nil)
	}
}

// mcpVersionSupported reports whether version is a revision this endpoint
// implements.
func mcpVersionSupported(version string) bool {
	return version == mcpProtocolVersionModern || version == mcpProtocolVersionLegacy
}

// mcpInitializeResult answers the classic handshake. The client's requested
// revision is echoed when it is one this server speaks, and the modern revision
// is offered otherwise.
func (s *Server) mcpInitializeResult(params json.RawMessage) map[string]any {
	requested := ""
	if len(params) > 0 {
		var payload struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &payload); err == nil {
			requested = strings.TrimSpace(payload.ProtocolVersion)
		}
	}
	version := mcpProtocolVersionLegacy
	if mcpVersionSupported(requested) {
		version = requested
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "llama-swap",
			"version": s.build.Version,
		},
		"instructions": mcpInstructions,
	}
}

// mcpDiscoverResult answers the stateless revision's opening RPC.
func (s *Server) mcpDiscoverResult() map[string]any {
	return map[string]any{
		"supportedVersions": []string{mcpProtocolVersionModern, mcpProtocolVersionLegacy},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"instructions": mcpInstructions,
		"ttlMs":        toolListTTLMs,
		"cacheScope":   "public",
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": map[string]any{
				"name":    "llama-swap",
				"version": s.build.Version,
			},
		},
	}
}

const mcpInstructions = "Tools are namespaced by what they answer about. docs__list, docs__search and " +
	"docs__get read llama-swap's own documentation: list the index, find a section by keyword, then read " +
	"it. config__get answers a jq query against the configuration this server is running right now with " +
	"credentials redacted, which is the right source for advice about the active deployment. " +
	"sys__overview reports the build, the configured models and the host hardware."

// handleMCPToolCall dispatches one tools/call.
func (s *Server) handleMCPToolCall(w http.ResponseWriter, r *http.Request, req mcpRequest, provider *docagent.Provider) {
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeMCPError(w, req.ID, codeMCPInvalidParams, "tools/call params are not an object", nil)
			return
		}
	}
	if strings.TrimSpace(params.Name) == "" {
		writeMCPError(w, req.ID, codeMCPInvalidParams, "tools/call requires a tool name", nil)
		return
	}

	result := provider.Call(r.Context(), params.Name, params.Arguments)
	writeMCPResult(w, req.ID, mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: result.Content}},
		IsError: result.IsError,
	})
}

// isMCPNotification reports whether an id is absent, which makes the message a
// notification. A JSON null id is a valid request id, so only a missing one
// counts.
func isMCPNotification(id json.RawMessage) bool {
	return len(id) == 0
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, message string, data json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: message, Data: data},
	})
}

// SetDocumentation indexes fsys and installs the /api/mcp tools. It is
// separate from New so embedders that construct a Server directly keep the old
// constructor signature, and so a Server built without documentation answers
// the endpoint with "not available in this build" instead of failing.
func (s *Server) SetDocumentation(fsys fs.FS) {
	if s == nil || fsys == nil {
		return
	}
	docs := docagent.New(fsys)
	s.mcpProvider = docagent.NewProvider(docs, mcpEnvironment{server: s})
	if s.proxylog != nil {
		s.proxylog.Debugf("documentation tools: %s indexed for /api/mcp", docs)
	}
}

// mcpEnvironment exposes the running server to the documentation tools.
type mcpEnvironment struct{ server *Server }

// RedactedConfigYAML returns the configuration this process is running, with
// credentials replaced by placeholders. It reads the same source-aware
// snapshot the config API serves, so the agent's view and the settings UI can
// never disagree.
func (e mcpEnvironment) RedactedConfigYAML(ctx context.Context) (string, error) {
	if e.server == nil {
		return "", fmt.Errorf("no server")
	}
	if manager := e.server.currentConfigManager(); manager != nil {
		snapshot, err := manager.Snapshot(ctx)
		if err != nil {
			return "", err
		}
		return snapshot.YAML, nil
	}
	// A server built without a config manager (embedders, tests) still has an
	// effective configuration; render that instead of failing.
	rendered, err := yaml.Marshal(e.server.currentConfig())
	if err != nil {
		return "", err
	}
	return string(rendered), nil
}

// ServerFacts describes the running instance.
func (e mcpEnvironment) ServerFacts(ctx context.Context) map[string]any {
	facts := map[string]any{}
	if e.server == nil {
		return facts
	}
	cfg := e.server.currentConfig()
	running := map[string]string{}
	if local := e.server.local; local != nil {
		for model, state := range local.RunningModels() {
			running[model] = string(state)
		}
	}
	facts["version"] = e.server.build.Version
	facts["commit"] = e.server.build.Commit
	facts["models"] = len(cfg.Models)
	facts["peers"] = len(cfg.Peers)
	facts["providers"] = len(cfg.Providers)
	facts["running"] = running
	if gateway := e.server.currentGateway(); gateway != nil {
		facts["providerModels"] = len(gateway.AllRoutes())
	}
	if e.server.hardware != nil {
		facts["hardware"] = e.server.hardware
	}
	return facts
}
