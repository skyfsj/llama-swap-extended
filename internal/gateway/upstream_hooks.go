package gateway

import (
	"bytes"
	"io"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func routeScope(route *Route, model string) conversation.Scope {
	format := dialect.OpenAIChat
	switch route.Type() {
	case config.ProviderTypeOpenAIResponses:
		format = dialect.OpenAIResponses
	case config.ProviderTypeAnthropic:
		format = dialect.ClaudeMessages
	case config.ProviderTypeGemini:
		format = dialect.Gemini
	case config.ProviderTypeGeminiInteractions:
		format = dialect.GeminiInteractions
	}
	return conversation.Scope{Provider: route.ProviderID, Adapter: route.AdapterName, Model: model, Format: format}
}

type upstreamCapture struct {
	body      bytes.Buffer
	truncated bool
}

func (c *upstreamCapture) Write(data []byte) (int, error) {
	n := len(data)
	left := (16 << 20) - c.body.Len()
	if len(data) > left {
		data = data[:left]
		c.truncated = true
	}
	_, _ = c.body.Write(data)
	return n, nil
}

type capturedBody struct {
	io.Reader
	io.Closer
}

func requestBytes(r *http.Request) []byte {
	if r.GetBody == nil {
		return nil
	}
	body, err := r.GetBody()
	if err != nil {
		return nil
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return nil
	}
	return data
}
