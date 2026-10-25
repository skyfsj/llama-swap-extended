package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func liveProtocolRequest(ctx context.Context, format dialect.Format, stream bool, body []byte) (*http.Request, *httptest.ResponseRecorder) {
	r := httptest.NewRequest(http.MethodPost, protocolFixturePath(format, stream), bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	return r, httptest.NewRecorder()
}

func TestServer_ProtocolLiveOpenCode(t *testing.T) {
	key := os.Getenv("OPENCODE_LIVE_KEY")
	if key == "" {
		t.Skip("OPENCODE_LIVE_KEY is unset")
	}
	for _, upstream := range []struct {
		format          dialect.Format
		endpoint, model string
	}{
		{dialect.OpenAIChat, "https://opencode.ai/inference/go/openai", "glm-5.3-flash"},
		{dialect.OpenAIResponses, "https://opencode.ai/inference/go/openai", "gpt-6-luna"},
		{dialect.ClaudeMessages, "https://opencode.ai/inference/go/anthropic", "claude-haiku-5-5"},
		{dialect.Gemini, "https://opencode.ai/inference/go/google", "gemini-4-argon-contributor"},
	} {
		t.Run(string(upstream.format), func(t *testing.T) {
			cfg := config.Config{Providers: config.ProviderDictionaryConfig{"live": {Headers: map[string]string{"User-Agent": "llama-swap-protocol-audit/1.0", "x-opencode-session": "llama-swap-protocol-audit-20261019"}, ApiKeys: []config.ProviderKey{{Value: key}}, Adapters: []config.ProviderAdapter{{Name: "live", Type: protocolFixtureSurface(upstream.format), Endpoint: upstream.endpoint, Models: []config.ProviderModel{{Local: "m", Remote: upstream.model}}}}}}}
			s := multiSurfaceServer(t, cfg)
			for _, client := range protocolFixtureFormats {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", client, stream), func(t *testing.T) {
						budget := 1024
						if value := os.Getenv("OPENCODE_LIVE_MAX_TOKENS"); value != "" {
							parsed, err := strconv.Atoi(value)
							if err != nil || parsed < 1 || parsed > 4096 {
								t.Fatal("invalid OPENCODE_LIVE_MAX_TOKENS")
							}
							budget = parsed
						}
						request := &dialect.Request{Model: "m", Stream: stream, MaxTokens: &budget, Messages: []dialect.Message{{Role: dialect.RoleUser, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "Reply with exactly OK. Do not use reasoning."}}}}}
						body, _, err := dialect.EncodeRequest(request, client, dialect.ConvertOptions{})
						if err != nil {
							t.Fatal(err)
						}
						ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
						defer cancel()
						var nativeOutput []byte
						ctx = swaputil.WithUpstreamHooks(ctx, swaputil.UpstreamHooks{Complete: func(ctx context.Context, scope conversation.Scope, input, output []byte, headers http.Header, status int, truncated bool) {
							nativeOutput = append([]byte(nil), output...)
						}})
						r, w := liveProtocolRequest(ctx, client, stream, body)
						s.ServeHTTP(w, r)
						if w.Code != 200 {
							t.Fatalf("live HTTP %d: %s", w.Code, strings.ReplaceAll(w.Body.String(), key, "[redacted]"))
						}
						response, err := decodeProtocolResponse(client, w.Body.Bytes())
						if err != nil {
							t.Fatal(err)
						}
						text := ""
						for _, part := range response.Parts {
							if part.Kind == dialect.PartText {
								text += part.Text
							}
						}
						if strings.TrimSpace(text) != "OK" {
							t.Fatalf("unexpected text length=%d status=%s response=%s native=%s", len(text), response.Status, strings.ReplaceAll(w.Body.String(), key, "[redacted]"), strings.ReplaceAll(string(nativeOutput), key, "[redacted]"))
						}
					})
				}
			}
		})
	}
}

func TestServer_ProtocolLiveOpenCodeMCP(t *testing.T) {
	key := os.Getenv("OPENCODE_LIVE_KEY")
	if key == "" {
		t.Skip("OPENCODE_LIVE_KEY is unset")
	}
	remote, count := mcpFixture(t)
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"live": {Headers: map[string]string{"User-Agent": "llama-swap-protocol-audit/1.0", "x-opencode-session": "llama-swap-protocol-tools-20261019"}, ApiKeys: []config.ProviderKey{{Value: key}}, Adapters: []config.ProviderAdapter{{Name: "live", Type: config.ProviderTypeOpenAI, Endpoint: "https://opencode.ai/inference/go/openai", Models: []config.ProviderModel{{Local: "m", Remote: "glm-5.3-flash"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	for _, client := range protocolFixtureFormats {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", client, stream), func(t *testing.T) {
				before := count.Load()
				budget := 1024
				request := &dialect.Request{Model: "m", Stream: stream, MaxTokens: &budget, Messages: []dialect.Message{{Role: dialect.RoleUser, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "Call the available echo tool exactly once with q set to audit. After receiving the result, reply with exactly OK."}}}}}
				body, _, err := dialect.EncodeRequest(request, client, dialect.ConvertOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				r, w := liveProtocolRequest(ctx, client, stream, body)
				s.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("live tool HTTP %d: %s", w.Code, strings.ReplaceAll(w.Body.String(), key, "[redacted]"))
				}
				response, err := decodeProtocolResponse(client, w.Body.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				text := ""
				for _, part := range response.Parts {
					if part.Kind == dialect.PartText {
						text += part.Text
					}
				}
				if count.Load() != before+1 || strings.TrimSpace(text) != "OK" {
					t.Fatalf("tool calls=%d text=%q", count.Load()-before, text)
				}
			})
		}
	}
}
