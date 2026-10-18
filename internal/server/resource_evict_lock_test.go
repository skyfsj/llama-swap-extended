package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
)

// TestServer_EvictionDoesNotHoldAdmissionLock proves a slow backend unload does
// not serialize unrelated admissions: while one request is evicting, a second
// request that needs no eviction must still be admitted immediately.
func TestServer_EvictionDoesNotHoldAdmissionLock(t *testing.T) {
	local := newStubRouter([]string{"busy", "requested", "other"}, "")
	local.running = map[string]process.ProcessState{"busy": process.StateReady}

	unloadStarted := make(chan struct{})
	releaseUnload := make(chan struct{})
	var once sync.Once
	// A slow unload is exactly the lock hazard: it must run outside the
	// admission critical section.
	local.unloadFunc = func(timeout time.Duration, models ...string) {
		for _, model := range models {
			if model != "busy" {
				continue
			}
			once.Do(func() { close(unloadStarted) })
			<-releaseUnload
			local.running = map[string]process.ProcessState{}
		}
	}

	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg.ResourceBudget = config.ResourceBudgetConfig{VRAMMiB: 100, AutoEvict: true}
	s.cfg.Models = map[string]config.ModelConfig{
		"busy":      {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"requested": {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 80}}},
		"other":     {Backend: config.BackendConfig{Resources: config.ResourceConfig{VRAMMiB: 10}}},
	}
	s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: 100})

	evictDone := make(chan error, 1)
	go func() {
		evictDone <- s.prepareResourceLoadContext(context.Background(), "requested")
	}()

	select {
	case <-unloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("eviction never started")
	}

	// The admission lock must be free while the unload is blocked.
	admitted := make(chan error, 1)
	go func() {
		admitted <- s.prepareResourceLoadContext(context.Background(), "other")
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("independent admission failed while an eviction was in progress: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("independent admission blocked behind a slow eviction")
	}

	close(releaseUnload)
	select {
	case err := <-evictDone:
		if err != nil {
			t.Fatalf("eviction path failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("eviction path did not finish")
	}
}
