//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWakeUpVLLM(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		status := status
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
				t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		if err := wakeUpVLLM(ts.URL); err != nil {
			ts.Close()
			t.Fatalf("wakeUpVLLM rejected successful status %d: %v", status, err)
		}
		ts.Close()
	}

	// Test failure when server returns error
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts2.Close()

	if err := wakeUpVLLM(ts2.URL); err == nil {
		t.Errorf("wakeUpVLLM expected error for non-200 response")
	}
}

func TestSleepVLLMAcceptsNoContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sleep" || r.URL.Query().Get("level") != "1" {
			t.Errorf("unexpected sleep request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	if err := sleepVLLM(ts.URL, 1); err != nil {
		t.Fatalf("sleepVLLM rejected 204 response: %v", err)
	}
}

func TestSuccessfulControlStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		if !isSuccessfulControlStatus(status) {
			t.Fatalf("status %d rejected, want successful", status)
		}
	}
	for _, status := range []int{http.StatusContinue, http.StatusMultipleChoices, http.StatusBadRequest, http.StatusInternalServerError} {
		if isSuccessfulControlStatus(status) {
			t.Fatalf("status %d accepted, want failure", status)
		}
	}
}

func TestWaitForHealthy(t *testing.T) {
	// Test successful health check
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	if err := waitForHealthyWithPath(ts.URL, "/v1/models", 2*time.Second); err != nil {
		t.Fatalf("waitForHealthy failed: %v", err)
	}

	// Test timeout: server delays response longer than context timeout
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay 2 seconds
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts2.Close()

	err := waitForHealthyWithPath(ts2.URL, "/v1/models", 1*time.Second)
	if err == nil {
		t.Errorf("waitForHealthy expected timeout error")
		return
	}
	if err != context.DeadlineExceeded {
		t.Errorf("waitForHealthy expected context deadline exceeded, got %v", err)
	}
}

func TestWaitForHealthyRejectsNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if err := waitForHealthyWithPath("http://127.0.0.1:1", "/health", timeout); err != context.DeadlineExceeded {
			t.Fatalf("timeout %s returned %v, want context deadline exceeded", timeout, err)
		}
	}
}

func TestWaitForHealthyNormalizesURLAndPathSlashes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected normalized health path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := waitForHealthyWithPath(ts.URL+"/", "v1/models", time.Second); err != nil {
		t.Fatalf("normalized health URL failed: %v", err)
	}
}

func TestVLLMControlRequestsDoNotFollowRedirects(t *testing.T) {
	redirected := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/health", http.StatusFound)
	}))
	defer source.Close()

	if err := checkHealthy(source.URL, "/health"); err == nil {
		t.Fatal("redirecting health endpoint unexpectedly succeeded")
	}
	select {
	case <-redirected:
		t.Fatal("control client followed readiness redirect")
	default:
	}
}

func TestWaitForHealthyDoesNotFollowRedirects(t *testing.T) {
	redirected := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/health", http.StatusFound)
	}))
	defer source.Close()

	if err := waitForHealthyWithPath(source.URL, "/health", 20*time.Millisecond); err != context.DeadlineExceeded {
		t.Fatalf("redirecting readiness returned %v, want deadline", err)
	}
	select {
	case <-redirected:
		t.Fatal("readiness poll followed redirect")
	default:
	}
}

func TestSleepCommandMarshal(t *testing.T) {
	// We test the sleep command by checking the JSON marshaling we use in sleepCmd.
	// Since sleepCmd is not easily unit-testable without exposing more, we test the structure.
	body := map[string]int{"level": 1}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	expected := `{"level":1}`
	if string(data) != expected {
		t.Errorf("Expected %s, got %s", expected, string(data))
	}
}

func TestValidateSleepLevelMatchesVLLMSupportedLevels(t *testing.T) {
	for _, level := range []int{1, 2} {
		if err := validateSleepLevel(level); err != nil {
			t.Errorf("level %d rejected: %v", level, err)
		}
	}
	for _, level := range []int{-1, 0, 3} {
		if err := validateSleepLevel(level); err == nil {
			t.Errorf("level %d accepted, want unsupported-level error", level)
		}
	}
}

func TestValidateVLLMBaseURLRejectsAmbiguousTargets(t *testing.T) {
	for _, value := range []string{
		"",
		"vllm.internal:8000",
		"ftp://vllm.internal:8000",
		"http://user:secret@127.0.0.1:8000",
		"http://127.0.0.1:8000?token=secret",
		"http://127.0.0.1:8000/#health",
		"http://127.0.0.1:8000/health\nX-Injected: yes",
		"http://127.0.0.1:8000/%0ahealth",
		"http://127.0.0.1:8000/api/../health",
		"http://127.0.0.1:8000/zero\u200bwidth",
	} {
		if err := validateVLLMBaseURL(value); err == nil {
			t.Fatalf("ambiguous vLLM URL %q was accepted", value)
		}
	}
	for _, value := range []string{"http://127.0.0.1:8000", "https://vllm.internal:8443/base"} {
		if err := validateVLLMBaseURL(value); err != nil {
			t.Fatalf("valid vLLM URL %q rejected: %v", value, err)
		}
	}
}

func TestValidateVLLMHealthPathRejectsQueryAndControlData(t *testing.T) {
	for _, value := range []string{
		"/health?token=secret",
		"/health#fragment",
		"/health\r\nX-Injected: yes",
		"/health%3Ftoken=secret",
		"/%2568ealth%3Ftoken=secret",
		"/health/%2e%2e/admin",
		`/health\\admin`,
		"/health\u200bcheck",
	} {
		if err := validateVLLMHealthPath(value); err == nil {
			t.Fatalf("unsafe health path %q was accepted", value)
		}
	}
	if err := validateVLLMHealthPath("/v1/models"); err != nil {
		t.Fatalf("valid health path rejected: %v", err)
	}
}

func TestUnsafeVLLMPathDenylist(t *testing.T) {
	for _, path := range []string{
		"/sleep",
		"/wake_up",
		"/is_sleeping",
		"/pause",
		"/resume",
		"/is_paused",
		"/abort_requests",
		"/collective_rpc",
		"/collective_rpc/foo",
		"/load_lora_adapter",
		"/v1/load_lora_adapter",
		"/v1/unload_lora_adapter",
		"/reload_weights",
		"/reset_prefix_cache",
		"/reset_mm_cache",
		"/reset_encoder_cache",
		"/init_weight_transfer_engine",
		"/start_weight_update",
		"/update_weights",
		"/finish_weight_update",
		"/update_weight_version",
		"/weight_info",
		"/get_world_size",
		"/scale_elastic_ep",
		"/is_scaling_elastic_ep",
		"/server_info",
		"/tokenizer_info",
	} {
		if !isUnsafeVLLMPath(path) {
			t.Fatalf("path %q should be denied", path)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/collective_rpcx"} {
		if isUnsafeVLLMPath(path) {
			t.Fatalf("path %q should remain available or be handled separately", path)
		}
	}
}

func TestUnsafeVLLMPathDenylistNormalizesVariants(t *testing.T) {
	for _, path := range []string{
		"/SLEEP",
		"/sleep/",
		"//sleep",
		"/v1/../sleep",
		"/%73leep",
		"/%2573leep",
		`/\\sleep`,
		"/collective_rpc/%66oo",
	} {
		if !isUnsafeVLLMPath(path) {
			t.Fatalf("path variant %q should be denied", path)
		}
	}
	for _, path := range []string{"/sleepy", "/v1/collective_rpcx", "/v1/chat/completions"} {
		if isUnsafeVLLMPath(path) {
			t.Fatalf("path %q should remain available", path)
		}
	}
	if !isUnsafeVLLMPath("/%zz") {
		t.Fatal("malformed escaped path should fail closed")
	}
}

// TestStartDaemon tests that startDaemon returns an error when the start command exits
// quickly and the daemon does not become healthy.
func TestStartDaemon(t *testing.T) {
	// Use a start command that exits immediately (true) and a health URL that will not respond.
	err := startDaemon([]string{"true"}, "http://127.0.0.1:12345/health", "/health", 10*time.Millisecond)
	if err == nil {
		t.Fatalf("startDaemon expected error but got nil")
	}
	if !strings.Contains(err.Error(), "daemon did not become healthy") {
		t.Errorf("error expected to contain 'daemon did not become healthy', got %v", err)
	}
}

// TestStartDaemonArgv verifies that multiple startArgs are passed as separate argv values.
func TestStartDaemonArgv(t *testing.T) {
	tmpDir := t.TempDir()
	argvFile := filepath.Join(tmpDir, "argv.txt")

	script := filepath.Join(tmpDir, "write-argv.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/bash\nprintf '%s\n' \"$@\" > \""+argvFile+"\"\nexit 0\n",
	), 0755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	err := startDaemon([]string{script, "arg1", "arg2", "arg3"}, "http://127.0.0.1:12345/health", "/health", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error (health check fails)")
	}

	content, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(content)), "\n")
	want := []string{"arg1", "arg2", "arg3"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
