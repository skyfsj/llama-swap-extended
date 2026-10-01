package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRuntime_ProbeLMCacheEnvironmentToleratesMergedStderrNoise(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(name, "python") {
			t.Fatalf("probe must invoke the staged interpreter, got %q", name)
		}
		noise := []byte("\x1b[32mLoading lmcache runtime\x1b[0m\r\nWARNING: noisy import line\n")
		payload := []byte(`{"importable": true, "lmcache": "0.5.4", "pythonVersion": "3.11.16"}`)
		return append(noise, payload...), nil
	}
	got, err := probeLMCacheEnvironment(context.Background(), run, "/staging", "/staging/.venv/bin/python")
	if err != nil {
		t.Fatalf("probe with merged stderr noise must succeed, got: %v", err)
	}
	if got["lmcache"] != "0.5.4" {
		t.Fatalf("lmcache version = %q, want 0.5.4", got["lmcache"])
	}
	if got["pythonVersion"] != "3.11.16" {
		t.Fatalf("pythonVersion = %q, want 3.11.16", got["pythonVersion"])
	}
}

func TestRuntime_ProbeLMCacheEnvironmentFailsWithoutJSONPayload(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("\x1b[32mLoading lmcache runtime\x1b[0m\n"), nil
	}
	if _, err := probeLMCacheEnvironment(context.Background(), run, "/staging", "/staging/.venv/bin/python"); err == nil {
		t.Fatal("probe without a JSON payload must fail closed")
	}
}

func TestRuntime_ProbeJSONLineExtraction(t *testing.T) {
	const payload = `{"importable": true, "lmcache": "0.5.4", "pythonVersion": "3.11.16"}`
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{name: "clean", output: payload + "\n", want: payload},
		{name: "noise before", output: "banner line\n\x1b[0m\n" + payload + "\n", want: payload},
		{name: "noise after", output: payload + "\ntrailing warning\n", want: payload},
		{name: "multiple json lines", output: `{"stale": true}` + "\n" + payload + "\n", want: payload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(lmcacheProbeJSONLine([]byte(tc.output))); got != tc.want {
				t.Fatalf("lmcacheProbeJSONLine = %q, want %q", got, tc.want)
			}
		})
	}
	if got := lmcacheProbeJSONLine([]byte("no json here\n\x1b[0m\n")); got != nil {
		t.Fatalf("expected nil for noise-only output, got %q", string(got))
	}
	if got := lmcacheProbeJSONLine(nil); got != nil {
		t.Fatalf("expected nil for empty output, got %q", string(got))
	}
}

func TestRuntime_ProbeLMCacheEnvironmentStillFailsWhenNotImportable(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"importable": false, "lmcache": "0.5.4", "pythonVersion": "3.11.16"}` + "\n"), nil
	}
	if _, err := probeLMCacheEnvironment(context.Background(), run, "/staging", "/staging/.venv/bin/python"); err == nil || !strings.Contains(err.Error(), "not importable") {
		t.Fatalf("expected importable failure, got: %v", err)
	}
}

// TestRuntime_LMCacheServerProbeRetriesAfterEarlyExit covers the port-race
// retry: the first probe attempt's server dies before answering health (the
// signature of a lost pre-picked port), so the wrapper must retry once with
// fresh ports before reporting failure.
func TestRuntime_LMCacheServerProbeRetriesAfterEarlyExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script probe fixture is unix-only")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "attempts")
	script := "#!/bin/sh\ncount=$(($(cat " + counter + " 2>/dev/null || echo 0) + 1))\necho $count > " + counter + "\nexit 1\n"
	executable := filepath.Join(binDir, "lmcache")
	if err := os.WriteFile(executable, []byte(script), 0o750); err != nil {
		t.Fatal(err)
	}

	err := defaultLMCacheServerProbe(context.Background(), dir, 2*time.Second)
	var early *lmCacheProbeEarlyExitError
	if err == nil || !errors.As(err, &early) {
		t.Fatalf("probe error = %v, want an early-exit error after retries", err)
	}
	attempts, readErr := os.ReadFile(counter)
	if readErr != nil {
		t.Fatalf("probe attempts counter missing: %v", readErr)
	}
	if strings.TrimSpace(string(attempts)) != "2" {
		t.Fatalf("probe attempts = %q, want 2 (one lost race + one retry)", attempts)
	}
}
