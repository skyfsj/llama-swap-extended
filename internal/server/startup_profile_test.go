package server

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_New_OnStartupProfile(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.Profiles = map[string]config.ProfileConfig{
		"coding": {Pins: map[string]string{"llm-code": "model"}},
	}
	cfg.Hooks.OnStartup.Profile = "coding"
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	s, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{}, nil)
	if err != nil {
		t.Fatalf("New (startup profile): %v", err)
	}
	if got := s.ActiveProfile(); got != "coding" {
		t.Fatalf("ActiveProfile()=%q want %q", got, "coding")
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// A configuration reload returns profile selection to the configured startup
// profile, the same state a fresh start would have. Runtime selection is not
// persisted, so a reload must not silently keep an operator's earlier choice.
func TestServer_ReconcileConfigReactivatesStartupProfile(t *testing.T) {
	discard := logmon.NewWriter(io.Discard)
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.Profiles = map[string]config.ProfileConfig{
		"coding": {Pins: map[string]string{"llm-code": "any-model"}},
		"chat":   {Pins: map[string]string{"llm-code": "any-model"}},
	}
	cfg.Hooks.OnStartup.Profile = "coding"

	local, err := router.NewGroup(cfg, discard, discard)
	if err != nil {
		t.Fatalf("router.NewGroup: %v", err)
	}
	t.Cleanup(func() { _ = local.Shutdown(time.Second) })

	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	// newTestServerWithConfig bypasses New, which is what seeds the startup
	// profile into a freshly constructed server.
	s.activeProfile = cfg.Hooks.OnStartup.Profile
	if got := s.ActiveProfile(); got != "coding" {
		t.Fatalf("initial ActiveProfile()=%q want coding", got)
	}

	// An operator switches profile at runtime...
	if _, err := s.setActiveProfile("chat"); err != nil {
		t.Fatalf("setActiveProfile: %v", err)
	}
	if got := s.ActiveProfile(); got != "chat" {
		t.Fatalf("after switch ActiveProfile()=%q want chat", got)
	}

	// ...and a reload with an unrelated change returns to the configured
	// startup profile.
	desired := cfg
	desired.LogLevel = "debug"
	if err := s.ReconcileConfig(desired); err != nil {
		t.Fatalf("ReconcileConfig: %v", err)
	}
	if got := s.ActiveProfile(); got != "coding" {
		t.Fatalf("after reload ActiveProfile()=%q want coding", got)
	}
}
