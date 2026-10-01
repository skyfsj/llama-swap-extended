package process

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestProcessCommand_VLLMWarmupFallsBackThroughAliasNames(t *testing.T) {
	served := "qwen-high"
	var seen []string
	var mu sync.Mutex
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != vllmWarmupPath {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		var payload vllmWarmupRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen, payload.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if payload.Model == served {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"The model does not exist."}}`)
	})

	p := &ProcessCommand{id: "canonical", config: config.ModelConfig{
		UseModelName: "primary",
		Aliases:      []string{"alias-a", "qwen-high"},
		Backend: config.BackendConfig{
			Type:   "vllm",
			Launch: &config.ModelLaunchConfig{ServedModelName: "served-pinned"},
		},
	}}

	if err := p.warmupVLLM(context.Background(), upstream); err != nil {
		t.Fatalf("warmupVLLM: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	want := []string{"primary", "alias-a", "qwen-high"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("warmup tried %q, want %q (useModelName first, then aliases, stop at first accepted)", got, want)
	}
}

// TestProcessCommand_VLLMWarmupReportsNoServedName guards the failure mode
// behind "The model `<alias>` does not exist": when the engine serves none
// of the configured names the warmup must surface the mismatch (it stays
// non-fatal to the start, finishVLLMStart logs it) rather than pass
// silently under an unrelated name.
func TestProcessCommand_VLLMWarmupReportsNoServedName(t *testing.T) {
	var requests atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != vllmWarmupPath {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"The model does not exist."}}`)
	})

	p := &ProcessCommand{id: "canonical", config: config.ModelConfig{
		Aliases: []string{"alias-a"},
		Backend: config.BackendConfig{Type: "vllm"},
	}}

	err := p.warmupVLLM(context.Background(), upstream)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("warmupVLLM error=%v, want the upstream model-not-found message", err)
	}
	// useModelName empty: canonical, alias-a -> 2 distinct tries.
	if got := requests.Load(); got != 2 {
		t.Fatalf("warmup requests=%d, want 2", got)
	}
}

// TestProcessCommand_VLLMWarmupGenericFailureStopsFallback ensures a non
// model-not-found upstream error (e.g. chat disabled) aborts the fallback
// loop instead of burning through every configured name.
func TestProcessCommand_VLLMWarmupGenericFailureStopsFallback(t *testing.T) {
	var requests atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != vllmWarmupPath {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		http.Error(w, "chat is not enabled", http.StatusBadRequest)
	})

	p := &ProcessCommand{id: "canonical", config: config.ModelConfig{
		UseModelName: "primary",
		Aliases:      []string{"alias-a"},
		Backend:      config.BackendConfig{Type: "vllm"},
	}}

	err := p.warmupVLLM(context.Background(), upstream)
	if err == nil {
		t.Fatal("warmupVLLM succeeded, want the upstream error")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("warmup requests=%d, want 1 (generic failure must not fall back)", got)
	}
}
