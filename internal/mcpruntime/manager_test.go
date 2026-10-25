package mcpruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestRuntime_SessionIdentityAndReuse(t *testing.T) {
	t.Setenv("LLAMA_MCP_HELPER", "1")
	t.Setenv("LLAMA_MCP_STATEFUL", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := config.MCPServerConfig{Transport: "stdio", Command: exe, Args: []string{"-test.run=TestRuntime_StdioHelper"}, Env: []string{"LLAMA_MCP_HELPER", "LLAMA_MCP_STATEFUL"}, Approval: "never"}
	cfg := config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"stateful": server}}
	manager := NewManager(t.Context())
	defer manager.Close()
	ownerA := WithManager(t.Context(), manager, "owner-a")
	first, err := Discover(ownerA, cfg, "m")
	if err != nil || len(first) != 1 {
		t.Fatalf("discovery=%v err=%v", first, err)
	}
	result, err := first[0].Call(ownerA, map[string]any{})
	if err != nil || result.Content[0].(*mcp.TextContent).Text != first[0].Tool.Name {
		t.Fatalf("stateful tool call=%v err=%v", result, err)
	}
	var wait sync.WaitGroup
	names := make(chan string, 12)
	failures := make(chan error, 12)
	for range 12 {
		wait.Go(func() {
			bindings, err := Discover(ownerA, cfg, "m")
			if err != nil {
				failures <- err
				return
			}
			names <- bindings[0].Tool.Name
		})
	}
	wait.Wait()
	close(names)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for name := range names {
		if name != first[0].Tool.Name {
			t.Errorf("one owner received different session tool names: %s %s", name, first[0].Tool.Name)
		}
	}
	ownerB := WithManager(t.Context(), manager, "owner-b")
	other, err := Discover(ownerB, cfg, "m")
	if err != nil || len(other) != 1 {
		t.Fatalf("other discovery=%v err=%v", other, err)
	}
	if other[0].Tool.Name == first[0].Tool.Name {
		t.Fatal("MCP session leaked across identities")
	}
	if _, err := first[0].Call(ownerB, map[string]any{}); err == nil {
		t.Fatal("cross-owner tool identity unexpectedly matched")
	}
	manager.Reset()
	fresh, err := Discover(ownerA, cfg, "m")
	if err != nil || len(fresh) != 1 || fresh[0].Tool.Name == first[0].Tool.Name {
		t.Fatalf("configuration reset reused old session: %v %v", fresh, err)
	}
	manager.Close()
	if _, err := Discover(ownerA, cfg, "m"); err == nil {
		t.Fatal("closed runtime accepted a session")
	}
}

func TestRuntime_ManagerShutdownCancelsCalls(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "cancel", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	server.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	manager := NewManager(t.Context())
	defer manager.Close()
	ctx := WithManager(t.Context(), manager, "owner")
	session, err := Connect(ctx, config.MCPServerConfig{Transport: "http", URL: httpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result := make(chan error, 1)
	go func() {
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "wait", Arguments: map[string]any{}})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	manager.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown lost MCP cancellation")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled call returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release caller")
	}
}

func TestRuntime_SessionPinsAndReplacement(t *testing.T) {
	t.Setenv("LLAMA_MCP_HELPER", "1")
	t.Setenv("LLAMA_MCP_STATEFUL", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MCPServerConfig{Transport: "stdio", Command: exe, Args: []string{"-test.run=TestRuntime_StdioHelper"}, Env: []string{"LLAMA_MCP_HELPER", "LLAMA_MCP_STATEFUL"}}
	manager := NewManager(t.Context())
	manager.idle = 20 * time.Millisecond
	defer manager.Close()
	ctx := WithManager(t.Context(), manager, "owner")
	held, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := held.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := tools.Tools[0].Name
	if err := Pin(ctx, cfg, "approval", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	held.Close()
	time.Sleep(60 * time.Millisecond)
	pinned, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tools, err = pinned.ListTools(ctx, nil)
	if err != nil || tools.Tools[0].Name != original {
		t.Fatalf("approval did not pin the session: %v %v", tools, err)
	}
	pinned.ClientSession.Close()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		remaining := len(manager.sessions)
		manager.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed session remained in the active pool")
		}
		time.Sleep(time.Millisecond)
	}
	fresh, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tools, err = fresh.ListTools(ctx, nil)
	if err != nil || tools.Tools[0].Name == original {
		t.Fatalf("dead session was reused: %v %v", tools, err)
	}
	replacement := tools.Tools[0].Name
	pinned.Close()
	ReleasePin(ctx, "approval")
	again, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tools, err = again.ListTools(ctx, nil)
	if err != nil || tools.Tools[0].Name != replacement {
		t.Fatalf("old lease removed replacement: %v %v", tools, err)
	}
	fresh.Close()
	again.Close()
}

func TestRuntime_CredentialScopeKey(t *testing.T) {
	t.Setenv("MCP_KEY_A", "first")
	t.Setenv("MCP_KEY_B", "second")
	manager := NewManager(t.Context())
	defer manager.Close()
	cfg := config.MCPServerConfig{Transport: "http", URL: "https://example.com", Headers: map[string]string{"Authorization": "MCP_KEY_A", "X-Key": "MCP_KEY_B"}}
	ctx := WithManager(t.Context(), manager, "owner")
	key := ScopeKey(ctx, cfg)
	for range 100 {
		if ScopeKey(ctx, cfg) != key {
			t.Fatal("scope key depends on map iteration")
		}
	}
	if ScopeKey(WithManager(t.Context(), manager, "other"), cfg) == key {
		t.Fatal("scope key did not isolate owner")
	}
	t.Setenv("MCP_KEY_A", "rotated")
	if ScopeKey(ctx, cfg) == key {
		t.Fatal("scope key ignored credential rotation")
	}
}
