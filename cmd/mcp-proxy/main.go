package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
)

func run(ctx context.Context, endpoint, authorizationEnv string) error {
	cfg := config.MCPServerConfig{Transport: "http", URL: endpoint}
	if authorizationEnv != "" {
		cfg.Headers = map[string]string{"Authorization": authorizationEnv}
	}
	if err := (config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Servers: map[string]config.MCPServerConfig{"gateway": cfg}}}).Validate(); err != nil {
		return err
	}
	bridge, err := mcpruntime.NewBridge(ctx, map[string]config.MCPServerConfig{"gateway": cfg}, false, nil)
	if err != nil {
		return err
	}
	defer bridge.Close()
	return bridge.Server.Run(ctx, &mcp.StdioTransport{})
}

func main() {
	endpoint := flag.String("url", "", "gateway /mcp URL including model query parameter")
	authorizationEnv := flag.String("authorization-env", "", "environment variable containing the Authorization header value")
	flag.Parse()
	if *endpoint == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *endpoint, *authorizationEnv); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
