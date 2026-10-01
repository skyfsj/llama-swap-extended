package process

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestProcessCommand_VLLMWarmupAfterReadiness(t *testing.T) {
	skipIfNoSimpleResponder(t)

	var healthRequests atomic.Int32
	var warmupRequests atomic.Int32
	var warmupPayload vllmWarmupRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			healthRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		case vllmWarmupPath:
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "body", http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(body, &warmupPayload); err != nil {
				http.Error(w, "json", http.StatusBadRequest)
				return
			}
			warmupRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              backend.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UseModelName:       "client-model",
		Backend: config.BackendConfig{
			Type:   "vllm",
			Launch: &config.ModelLaunchConfig{ServedModelName: "served-model"},
		},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("state=%s, want %s", got, StateReady)
	}
	if got := warmupRequests.Load(); got != 1 {
		t.Fatalf("warmup requests=%d, want 1", got)
	}
	if got := healthRequests.Load(); got == 0 {
		t.Fatal("readiness check was not sent")
	}
	if warmupPayload.Model != "client-model" {
		t.Errorf("warmup model=%q, want client-model", warmupPayload.Model)
	}
	if len(warmupPayload.Messages) != 1 || warmupPayload.Messages[0].Role != "user" || warmupPayload.Messages[0].Content != vllmWarmupPrompt {
		t.Errorf("warmup messages=%+v, want one user %q message", warmupPayload.Messages, vllmWarmupPrompt)
	}
	if warmupPayload.MaxTokens != 1 || warmupPayload.Temperature != 0 || warmupPayload.Stream {
		t.Errorf("warmup parameters=%+v, want max_tokens=1 temperature=0 stream=false", warmupPayload)
	}

	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("second EnsureReady: %v", err)
	}
	if got := warmupRequests.Load(); got != 1 {
		t.Fatalf("warmup requests after idempotent EnsureReady=%d, want 1", got)
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady after stop: %v", err)
	}
	if got := warmupRequests.Load(); got != 2 {
		t.Fatalf("warmup requests after restart=%d, want 2", got)
	}
}

func TestProcessCommand_VLLMWarmupFailureDoesNotBlockReady(t *testing.T) {
	skipIfNoSimpleResponder(t)

	var warmupRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == vllmWarmupPath {
			warmupRequests.Add(1)
			http.Error(w, "chat is not enabled", http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(backend.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              backend.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		Backend:            config.BackendConfig{Type: "vllm"},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("state=%s, want %s", got, StateReady)
	}
	if got := warmupRequests.Load(); got != 1 {
		t.Fatalf("warmup requests=%d, want 1", got)
	}
}

func TestProcessCommand_VLLMWarmupStopCanRestart(t *testing.T) {
	skipIfNoSimpleResponder(t)

	warmupStarted := make(chan struct{}, 1)
	var warmupRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case vllmWarmupPath:
			if warmupRequests.Add(1) == 1 {
				select {
				case warmupStarted <- struct{}{}:
				default:
				}
				<-time.After(100 * time.Millisecond)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)

	cmd, _ := simpleResponderCmd(t, "-silent")
	conf := config.ModelConfig{
		Cmd:                cmd,
		Proxy:              backend.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		Backend:            config.BackendConfig{Type: "vllm"},
	}
	p := newProcessCommand(t, conf)
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.EnsureReady(context.Background(), testStartTimeout)
	}()
	select {
	case <-warmupStarted:
	case <-time.After(testStartTimeout):
		t.Fatal("vLLM warmup did not start")
	}

	stopErr := make(chan error, 1)
	go func() {
		stopErr <- p.Stop(testStopTimeout)
	}()
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(testStopTimeout):
		t.Fatal("Stop did not return while warmup was in flight")
	}
	select {
	case err := <-startErr:
		if !errors.Is(err, ErrStartAborted) {
			t.Fatalf("EnsureReady error=%v, want ErrStartAborted", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("EnsureReady did not return after Stop")
	}
	if got := p.State(); got != StateStopped {
		t.Fatalf("state=%s, want %s after cancelled warmup", got, StateStopped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady after cancelled warmup: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("state=%s, want %s after restart", got, StateReady)
	}
	if got := warmupRequests.Load(); got != 2 {
		t.Fatalf("warmup requests=%d, want 2 after restart", got)
	}
}

func TestProcessCommand_VLLMWarmupSkipsOtherBackends(t *testing.T) {
	p := &ProcessCommand{config: config.ModelConfig{Backend: config.BackendConfig{Type: "llamacpp"}}}
	var requests atomic.Int32
	upstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	})

	if err := p.warmupVLLM(context.Background(), upstream); err != nil {
		t.Fatalf("warmupVLLM: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests=%d, want 0", got)
	}
	if strings.TrimSpace(p.config.Backend.Type) != "llamacpp" {
		t.Fatal("test backend type changed")
	}
}
