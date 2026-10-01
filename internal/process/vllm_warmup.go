package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"time"
)

const (
	vllmWarmupPath    = "/v1/chat/completions"
	vllmWarmupPrompt  = "warmup"
	vllmWarmupTimeout = 30 * time.Second
)

type vllmWarmupMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type vllmWarmupRequest struct {
	Model       string              `json:"model"`
	Messages    []vllmWarmupMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature float64             `json:"temperature"`
	Stream      bool                `json:"stream"`
}

// finishVLLMStart runs the best-effort vLLM warmup after the configured
// readiness check has passed and before the process is published as ready.
// A warmup failure is deliberately non-fatal: some vLLM deployments expose a
// non-chat model or a custom API surface, but the backend can still serve its
// configured requests. Cancellation of the actual start, however, remains an
// aborted start and must not publish a dead process.
func (p *ProcessCommand) finishVLLMStart(
	startCtx context.Context,
	cmd *exec.Cmd,
	cmdDone chan struct{},
	cmdCancel context.CancelFunc,
	handlerFn http.HandlerFunc,
	reverseProxy http.Handler,
	prematureExit func() startResult,
) startResult {
	if p.isVLLMBackend() {
		if err := p.warmupVLLM(startCtx, reverseProxy); err != nil {
			if startCtx.Err() != nil {
				return prematureExit()
			}
			p.proxyLogger.Warnf("<%s> vLLM warmup failed: %v", p.id, err)
		} else {
			p.proxyLogger.Infof("<%s> vLLM warmup completed", p.id)
		}
	}

	select {
	case <-cmdDone:
		return prematureExit()
	default:
	}
	return startResult{cmd: cmd, cmdDone: cmdDone, cancel: cmdCancel, handlerFn: handlerFn}
}

func (p *ProcessCommand) isVLLMBackend() bool {
	return p != nil && strings.EqualFold(strings.TrimSpace(p.config.Backend.Type), "vllm")
}

// errUpstreamModelUnknown classifies warmup failures that mean the upstream
// rejected the candidate model name, so the caller can fall back to another
// accepted name.
var errUpstreamModelUnknown = errors.New("upstream does not serve this model name")

// warmupVLLM runs the warmup once per name the engine is configured to
// accept, in order of likelihood: useModelName first, then the configured
// aliases, then the manually pinned servedModelName, and finally the
// canonical model ID. llama-swap does not rewrite alias requests before
// forwarding them upstream, so the engine must be reachable under at least
// one of these names for alias traffic to work; probing each name makes the
// warmup report that misconfiguration at start time instead of silently
// warming up under an unrelated name while every aliased request 404s.
func (p *ProcessCommand) warmupVLLM(ctx context.Context, upstream http.Handler) error {
	if !p.isVLLMBackend() {
		return nil
	}
	if upstream == nil {
		return fmt.Errorf("vLLM warmup upstream is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	names := make([]string, 0, 3+len(p.config.Aliases))
	names = append(names, strings.TrimSpace(p.config.UseModelName))
	names = append(names, p.config.Aliases...)
	if p.config.Backend.Launch != nil {
		names = append(names, strings.TrimSpace(p.config.Backend.Launch.ServedModelName))
	}
	names = append(names, p.id)
	unique := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}

	var lastErr error
	for i, name := range unique {
		var err error
		var sent string
		sent, err = p.warmupVLLMModel(ctx, upstream, name)
		if err == nil {
			if i > 0 && p.proxyLogger != nil {
				p.proxyLogger.Infof("<%s> vLLM warmup accepted upstream model name %q", p.id, sent)
			}
			return nil
		}
		lastErr = err
		if !errors.Is(err, errUpstreamModelUnknown) {
			return err
		}
	}
	return lastErr
}

// warmupVLLMModel sends one best-effort warmup request for the named upstream
// model and returns the name that was actually sent. A model-not-found style
// response maps to errUpstreamModelUnknown so the caller can fall back to
// another accepted name; any other failure is returned unchanged.
func (p *ProcessCommand) warmupVLLMModel(ctx context.Context, upstream http.Handler, model string) (string, error) {
	payload, err := json.Marshal(vllmWarmupRequest{
		Model: model,
		Messages: []vllmWarmupMessage{{
			Role:    "user",
			Content: vllmWarmupPrompt,
		}},
		MaxTokens:   1,
		Temperature: 0,
		Stream:      false,
	})
	if err != nil {
		return model, fmt.Errorf("encode vLLM warmup request: %w", err)
	}

	warmupCtx, cancel := context.WithTimeout(ctx, vllmWarmupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(warmupCtx, http.MethodPost, vllmWarmupPath, bytes.NewReader(payload))
	if err != nil {
		return model, fmt.Errorf("create vLLM warmup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	recorder := httptest.NewRecorder()
	defer req.Body.Close()
	upstream.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if err != nil {
		return model, fmt.Errorf("read vLLM warmup response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = "<empty body>"
		}
		if looksLikeUnknownModel(response.StatusCode, body) {
			return model, fmt.Errorf("vLLM warmup with model %q: %w (%s)", model, errUpstreamModelUnknown, detail)
		}
		return model, fmt.Errorf("vLLM warmup with model %q returned HTTP %d: %s", model, response.StatusCode, detail)
	}
	return model, nil
}

// looksLikeUnknownModel recognizes the vLLM and llama.cpp "model does not
// exist" errors so the warmup can try the next accepted name instead of
// treating a name misconfiguration as a generic upstream failure.
func looksLikeUnknownModel(status int, body []byte) bool {
	if status != http.StatusNotFound && status != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "does not exist") ||
		strings.Contains(text, "not found") ||
		strings.Contains(text, "unknown model") ||
		strings.Contains(text, "model_not_found")
}
