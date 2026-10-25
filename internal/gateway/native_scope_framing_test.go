package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestGateway_ForwardScopeCancelFraming pins the wire shape of the cancel
// request: a POST with no body. An upstream that does not implement chunked
// request decoding (some simple MCP/probe servers do not) would otherwise see
// the terminating chunk as a second, malformed request.
func TestGateway_ForwardScopeCancelFraming(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	head := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var lines []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
			lines = append(lines, strings.TrimSpace(line))
		}
		head <- strings.Join(lines, "\n")
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	}()

	cfg := config.Config{Providers: config.ProviderDictionaryConfig{"p": {
		Type:     config.ProviderTypeOpenAIResponses,
		Endpoint: "http://" + listener.Addr().String(),
		ApiKeys:  []config.ProviderKey{{Value: "k"}},
		Models:   []config.ProviderModel{{Local: "m", Remote: "m"}},
	}}}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Shutdown()

	scope := conversation.Scope{Provider: "p", Model: "m", Credential: keyID("p", "k"), Format: dialect.OpenAIResponses}
	w := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://gateway.internal/v1/responses/resp_1/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("request body nil=%v CL=%d TE=%v", req.Body == nil, req.ContentLength, req.TransferEncoding)
	if err := g.ForwardScope(w, req, scope, Session{}); err != nil {
		t.Fatalf("ForwardScope: %v", err)
	}
	select {
	case got := <-head:
		if strings.Contains(strings.ToLower(got), "transfer-encoding: chunked") {
			t.Fatalf("cancel request is chunked; a peer that does not decode chunked bodies sees a stray terminating chunk:\n%s", got)
		}
	case <-timeoutAfter():
		t.Fatal("no request reached the listener")
	}
}

func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }
