package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type mcpLiveBridge struct {
	bridge   *mcpruntime.Bridge
	handler  http.Handler
	lastUsed time.Time
	active   int
}

func (s *Server) serveMCPLive(w http.ResponseWriter, r *http.Request, cfg config.Config, model string) {
	configs := map[string]config.MCPServerConfig{}
	versions := map[string]string{}
	for id, remote := range cfg.ProtocolRuntime.MCP.Servers {
		if mcpruntime.Allowed(remote.Models, model) {
			configs[id] = remote
			versions[id] = mcpruntime.ScopeKey(r.Context(), remote)
		}
	}
	encoded, _ := json.Marshal([]any{identityIDFromContext(r.Context()), model, configs, versions})
	key := fmt.Sprintf("%x", sha256.Sum256(encoded))
	s.mcpBridgeMu.Lock()
	if s.mcpBridgesClosed {
		s.mcpBridgeMu.Unlock()
		swaputil.SendResponse(w, r, 503, "MCP runtime is closed")
		return
	}
	if s.mcpBridges == nil {
		s.mcpBridges = map[string]*mcpLiveBridge{}
	}
	entry := s.mcpBridges[key]
	if entry == nil {
		for id, candidate := range s.mcpBridges {
			if candidate.active == 0 && time.Since(candidate.lastUsed) > 30*time.Minute {
				delete(s.mcpBridges, id)
				candidate.bridge.Close()
			}
		}
		if len(s.mcpBridges) >= 64 {
			s.mcpBridgeMu.Unlock()
			swaputil.SendResponse(w, r, 503, "MCP bridge capacity exceeded")
			return
		}
		ctx := context.WithoutCancel(r.Context())
		bridge, err := mcpruntime.NewBridge(ctx, configs, true, func(ctx context.Context, b mcpruntime.Binding, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			if b.Config.Approval == "never" {
				return nil, nil
			}
			b.BridgeID = key
			b.Continuation = params
			result, err := s.callMCP(ctx, b, params.Arguments, model)
			var approval *mcpApprovalRequired
			if errors.As(err, &approval) {
				data, _ := json.Marshal(mcpWaitDetails(approval, err))
				return nil, &jsonrpc.Error{Code: -32001, Message: err.Error(), Data: data}
			}
			return result, err
		}, mcpruntime.WithTaskRegistry(mcpTaskRegistry{store: s.store, owner: identityIDFromContext(r.Context())}))
		if err != nil {
			s.mcpBridgeMu.Unlock()
			swaputil.SendResponse(w, r, 502, err.Error())
			return
		}
		getServer := func(*http.Request) *mcp.Server { return bridge.Server }
		legacy := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{PropagateRequestCancellation: true, SessionTimeout: 30 * time.Minute})
		modern := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("MCP-Protocol-Version") >= "2026-07-28" {
				modern.ServeHTTP(w, r)
			} else {
				legacy.ServeHTTP(w, r)
			}
		})
		entry = &mcpLiveBridge{bridge: bridge, handler: handler, lastUsed: time.Now()}
		s.mcpBridges[key] = entry
	}
	entry.active++
	entry.lastUsed = time.Now()
	s.mcpBridgeMu.Unlock()
	defer func() { s.mcpBridgeMu.Lock(); entry.active--; entry.lastUsed = time.Now(); s.mcpBridgeMu.Unlock() }()
	entry.handler.ServeHTTP(w, r)
}

type mcpTaskRegistry struct {
	store interface {
		SaveMCPTaskRoute(context.Context, string, string, []byte, time.Time) error
		LoadMCPTaskRoute(context.Context, string, string) ([]byte, error)
	}
	owner string
}

func (r mcpTaskRegistry) Save(ctx context.Context, id string, data []byte, expires time.Time) error {
	return r.store.SaveMCPTaskRoute(ctx, id, r.owner, data, expires)
}
func (r mcpTaskRegistry) Load(ctx context.Context, id string) ([]byte, error) {
	return r.store.LoadMCPTaskRoute(ctx, id, r.owner)
}

func (s *Server) closeMCPBridges() {
	s.mcpBridgeMu.Lock()
	s.mcpBridgesClosed = true
	bridges := s.mcpBridges
	s.mcpBridges = nil
	s.mcpBridgeMu.Unlock()
	for _, entry := range bridges {
		for peer := range entry.bridge.Server.Sessions() {
			peer.Close()
		}
		entry.bridge.Close()
	}
}

func (s *Server) executeMCPBinding(ctx context.Context, b mcpruntime.Binding, args any) (*mcp.CallToolResult, error) {
	if b.BridgeID == "" {
		return b.Call(ctx, args)
	}
	s.mcpBridgeMu.Lock()
	entry := s.mcpBridges[b.BridgeID]
	if entry != nil {
		entry.active++
		entry.lastUsed = time.Now()
	}
	s.mcpBridgeMu.Unlock()
	if entry == nil {
		return nil, errors.New("MCP approval session is no longer available")
	}
	defer func() { s.mcpBridgeMu.Lock(); entry.active--; entry.lastUsed = time.Now(); s.mcpBridgeMu.Unlock() }()
	params := &mcp.CallToolParams{Name: b.Tool.Name, Arguments: args}
	if b.Continuation != nil {
		*params = *b.Continuation
		params.Name = b.Tool.Name
		params.Arguments = args
	}
	return entry.bridge.Call(ctx, b.Server, params)
}
